package sharedbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/httpjson"
)

// Browser 是桌面窗口入口的受限能力，不包含浏览器全局协议或任意脚本执行。
type Browser interface {
	Open(context.Context, Address) (Window, error)
	List(context.Context, computer.ExecutionSession) ([]Window, error)
	Execute(context.Context, Address, Command) (Window, error)
	Screenshot(context.Context, Address) ([]byte, error)
	RespondDialog(context.Context, Address, DialogReply) error
	CloseWindow(context.Context, Address) error
}

// Peer 来自 Unix 连接的内核凭据，不能由请求头或 JSON 声明。
type Peer struct {
	PID      int
	UID, GID uint32
}

// PeerCheck 每次请求复验当前会话与进程资源组；连接建立时的成功不能替代租约检查。
type PeerCheck func(context.Context, computer.ExecutionSession, Peer) error

type GatewayOptions struct {
	Width, Height                int
	MaximumConnections           int
	CallTimeout, ShutdownTimeout time.Duration
}

func (options GatewayOptions) validate() error {
	if options.Width < 320 || options.Width > 1280 || options.Height < 240 || options.Height > 800 || options.MaximumConnections < 2 || options.MaximumConnections > 16 || options.CallTimeout < time.Second || options.CallTimeout > 30*time.Second || options.ShutdownTimeout < time.Second || options.ShutdownTimeout > time.Minute {
		return ErrInvalid
	}
	return nil
}

// Gateway 固定绑定单个完整执行会话，只由受信电脑代理交给所属桌面使用。
type Gateway struct {
	browser  Browser
	session  computer.ExecutionSession
	check    PeerCheck
	options  GatewayOptions
	logger   *slog.Logger
	mu       sync.Mutex
	started  bool
	stopping bool
	requests sync.WaitGroup
}

func NewGateway(browser Browser, session computer.ExecutionSession, check PeerCheck, options GatewayOptions, logger *slog.Logger) (*Gateway, error) {
	if browser == nil || session.Validate() != nil || check == nil || options.validate() != nil || logger == nil {
		return nil, ErrInvalid
	}
	return &Gateway{browser: browser, session: session, check: check, options: options, logger: logger}, nil
}

type peerKey struct{}

// Serve 接管专属 Unix 监听器；取消关闭全部连接并等待请求退出，不接收 TCP 或任意监听实现。
// 路径创建、私有父目录及 socket 身份复验由监督器负责；本方法不删除可能被替换的路径。
func (gateway *Gateway) Serve(ctx context.Context, listener *net.UnixListener) error {
	if listener == nil || listener.Addr().Network() != "unix" {
		return ErrInvalid
	}
	gateway.mu.Lock()
	if gateway.started {
		gateway.mu.Unlock()
		return ErrConflict
	}
	gateway.started = true
	gateway.mu.Unlock()
	listener.SetUnlinkOnClose(false)
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	limited := &gatewayListener{UnixListener: listener, slots: make(chan struct{}, gateway.options.MaximumConnections), gateway: gateway}
	server := &http.Server{Handler: http.HandlerFunc(gateway.handle), ReadHeaderTimeout: gateway.options.CallTimeout, ReadTimeout: gateway.options.CallTimeout, WriteTimeout: gateway.options.CallTimeout, IdleTimeout: gateway.options.CallTimeout, MaxHeaderBytes: 4096,
		BaseContext: func(net.Listener) context.Context { return life },
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			if owned, ok := connection.(*gatewayConnection); ok {
				return context.WithValue(ctx, peerKey{}, owned.peer)
			}
			return ctx
		},
	}
	// 每次调用使用单独连接，避免桌面重启后复用旧执行权或把迟到响应交给下一条命令。
	server.SetKeepAlivesEnabled(false)
	ended := make(chan error, 1)
	go func() { ended <- server.Serve(limited) }()
	var serveError error
	select {
	case <-ctx.Done():
	case serveError = <-ended:
	}
	gateway.mu.Lock()
	gateway.stopping = true
	gateway.mu.Unlock()
	cancel()
	closeError := server.Close()
	bounded, stop := context.WithTimeout(context.Background(), gateway.options.ShutdownTimeout)
	defer stop()
	if serveError == nil {
		select {
		case serveError = <-ended:
		case <-bounded.Done():
			return errors.Join(ErrUnavailable, closeError)
		}
	}
	if errors.Is(serveError, http.ErrServerClosed) {
		serveError = nil
	}
	drained := make(chan struct{})
	go func() { limited.pending.Wait(); gateway.requests.Wait(); close(drained) }()
	select {
	case <-drained:
		return errors.Join(serveError, closeError)
	case <-bounded.Done():
		return errors.Join(ErrUnavailable, serveError, closeError)
	}
}

type gatewayListener struct {
	*net.UnixListener
	slots   chan struct{}
	pending sync.WaitGroup
	gateway *Gateway
}
type gatewayConnection struct {
	net.Conn
	peer       Peer
	owner      *gatewayListener
	once       sync.Once
	closeError error
}

func (connection *gatewayConnection) Close() error {
	connection.once.Do(func() {
		connection.closeError = connection.Conn.Close()
		<-connection.owner.slots
		connection.owner.pending.Done()
	})
	return connection.closeError
}
func (listener *gatewayListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		peer, err := gatewayPeer(connection)
		if err == nil && peer.UID == uint32(listener.gateway.session.UID) && peer.GID == uint32(listener.gateway.session.GID) {
			select {
			case listener.slots <- struct{}{}:
				listener.pending.Add(1)
				return &gatewayConnection{Conn: connection, peer: peer, owner: listener}, nil
			default:
				listener.gateway.log("浏览器桌面入口连接数已满")
			}
		}
		if connection.Close() != nil {
			return nil, ErrUnavailable
		}
	}
}

func (gateway *Gateway) log(message string) {
	gateway.logger.Warn(message, "user_id", gateway.session.UserID, "computer_id", gateway.session.ComputerID, "bot_id", gateway.session.BotID, "session_id", gateway.session.SessionID)
}

func (gateway *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	gateway.mu.Lock()
	if gateway.stopping {
		gateway.mu.Unlock()
		gateway.failure(w, ErrUnavailable)
		return
	}
	gateway.requests.Add(1)
	gateway.mu.Unlock()
	defer gateway.requests.Done()
	ctx, cancel := context.WithTimeout(r.Context(), gateway.options.CallTimeout)
	defer cancel()
	peer, ok := r.Context().Value(peerKey{}).(Peer)
	if !ok || peer.PID < 1 || peer.UID != uint32(gateway.session.UID) || peer.GID != uint32(gateway.session.GID) || gateway.check(ctx, gateway.session, peer) != nil {
		gateway.failure(w, ErrDenied)
		return
	}
	if r.Host != "browser.local" || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.IsAbs() || r.Header.Get("Origin") != "" || r.Header.Get("Upgrade") != "" {
		gateway.failure(w, ErrInvalid)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			gateway.failure(w, ErrInvalid)
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v2/state" {
		windows, err := gateway.browser.List(ctx, gateway.session)
		if err != nil {
			gateway.failure(w, err)
			return
		}
		gateway.writeJSON(w, http.StatusOK, struct {
			Version int      `json:"version"`
			Width   int      `json:"width"`
			Height  int      `json:"height"`
			Windows []Window `json:"windows"`
		}{2, gateway.options.Width, gateway.options.Height, windows})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "v2" || parts[1] != "windows" {
		gateway.writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "浏览器接口不存在"})
		return
	}
	if _, err := ulid.ParseStrict(parts[2]); err != nil {
		gateway.failure(w, ErrInvalid)
		return
	}
	address := Address{Session: gateway.session, WindowID: parts[2]}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodPut:
			var empty struct{}
			if !gateway.decode(w, r, &empty) {
				return
			}
			window, err := gateway.browser.Open(ctx, address)
			gateway.result(w, window, err)
			return
		case http.MethodDelete:
			err := gateway.browser.CloseWindow(ctx, address)
			gateway.result(w, struct{}{}, err)
			return
		}
	} else {
		switch {
		case r.Method == http.MethodPost && parts[3] == "upload":
			uploader, ok := gateway.browser.(UploadBrowser)
			if !ok {
				gateway.failure(w, ErrUnavailable)
				return
			}
			if r.Header.Get("Content-Type") != "application/json" || r.ContentLength > maximumUploadBody {
				gateway.failure(w, ErrInvalid)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maximumUploadBody)
			window, err := uploader.Upload(ctx, address, r.Body)
			closeError := r.Body.Close()
			gateway.result(w, window, errors.Join(err, closeError))
			return
		case r.Method == http.MethodPost && parts[3] == "command":
			var command Command
			if !gateway.decode(w, r, &command) {
				return
			}
			window, err := gateway.browser.Execute(ctx, address, command)
			gateway.result(w, window, err)
			return
		case r.Method == http.MethodPost && parts[3] == "dialog":
			var reply DialogReply
			if !gateway.decode(w, r, &reply) {
				return
			}
			err := gateway.browser.RespondDialog(ctx, address, reply)
			gateway.result(w, struct{}{}, err)
			return
		case r.Method == http.MethodGet && parts[3] == "image":
			encoded, err := gateway.browser.Screenshot(ctx, address)
			if err != nil {
				gateway.failure(w, err)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			if _, err := w.Write(encoded); err != nil {
				gateway.log("浏览器桌面画面传输未完成")
			}
			return
		}
	}
	gateway.writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "浏览器接口不存在"})
}

func (gateway *Gateway) decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if failure := httpjson.Decode(r, value, 32768); failure != nil {
		gateway.failure(w, ErrInvalid)
		return false
	}
	return true
}
func (gateway *Gateway) result(w http.ResponseWriter, value any, err error) {
	if err != nil {
		gateway.failure(w, err)
		return
	}
	gateway.writeJSON(w, http.StatusOK, value)
}
func (gateway *Gateway) failure(w http.ResponseWriter, err error) {
	status, code, message := http.StatusServiceUnavailable, "unavailable", "浏览器服务不可用"
	switch {
	case errors.Is(err, ErrInvalid):
		status, code, message = http.StatusBadRequest, "invalid", "浏览器操作参数无效"
	case errors.Is(err, ErrDenied):
		status, code, message = http.StatusForbidden, "denied", "浏览器窗口授权已失效或不属于当前桌面"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "conflict", "浏览器窗口或操作状态已改变，请刷新后继续"
	case errors.Is(err, ErrCapacity):
		status, code, message = http.StatusTooManyRequests, "capacity", "浏览器窗口数量已达上限"
	case errors.Is(err, ErrUnknown):
		status, code, message = http.StatusConflict, "unknown", "浏览器操作结果未确认，窗口已停止接受新操作"
	case errors.Is(err, ErrDialog):
		status, code, message = http.StatusConflict, "dialog", "请先处理当前窗口的页面对话框"
	case errors.Is(err, ErrBusy):
		status, code, message = http.StatusConflict, "busy", "当前窗口动作尚未结束，请查询状态"
	case errors.Is(err, ErrNavigation):
		status, code, message = http.StatusUnprocessableEntity, "navigation", "页面导航失败或没有对应历史记录"
	}
	gateway.writeJSON(w, status, map[string]string{"code": code, "message": message})
}
func (gateway *Gateway) writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		gateway.log("浏览器桌面响应编码失败")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		gateway.log("浏览器桌面响应传输未完成")
	}
}
