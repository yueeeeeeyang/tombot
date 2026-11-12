package egressproxy

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// Owner 固定一个电脑实例，节点必须用实际容器父资源组验证 Peer，不能仅凭 root 身份放行。
type Owner struct {
	UserID, ComputerID, ContainerID string
	Generation                      int64
}
type Peer struct {
	PID      int32
	UID, GID uint32
}

// PeerCheck 在有界上下文内复验内核对端属于固定实例；不能信任 HTTP 头声明的身份。
type PeerCheck func(context.Context, Peer) error

type Server struct {
	options        Options
	owner          Owner
	check          PeerCheck
	logger         *slog.Logger
	resolver       resolver
	dialer         dialer
	localAddresses func() ([]netip.Addr, error)
	started        atomic.Bool
	mu             sync.Mutex
	closing        bool
	connections    map[*trackedConnection]struct{}
	slots          chan struct{}
	pending        sync.WaitGroup
	drained        chan struct{}
}

// New 创建固定实例的出网服务；不监听公网，不读取环境代理，也不保存网站请求正文或凭据。
func New(options Options, owner Owner, check PeerCheck, logger *slog.Logger) (*Server, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	for _, id := range []string{owner.UserID, owner.ComputerID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return nil, errors.New("出网电脑身份无效")
		}
	}
	if len(owner.ContainerID) != 64 || strings.Trim(owner.ContainerID, "0123456789abcdef") != "" || owner.Generation < 1 || check == nil || logger == nil {
		return nil, errors.New("出网实例或授权校验未配置")
	}
	options.Ports = slices.Clone(options.Ports)
	options.DeniedNetworks = slices.Clone(options.DeniedNetworks)
	return &Server{options: options, owner: owner, check: check, logger: logger, resolver: net.DefaultResolver, dialer: &net.Dialer{Timeout: options.ConnectTimeout}, localAddresses: localAddresses, connections: map[*trackedConnection]struct{}{}, slots: make(chan struct{}, options.MaximumConnections), drained: make(chan struct{})}, nil
}

type errorLog struct{ logger *slog.Logger }

func (writer errorLog) Write(encoded []byte) (int, error) {
	writer.logger.Warn("电脑出网 HTTP 连接处理失败", "diagnostic_bytes", len(encoded))
	return len(encoded), nil
}

// Serve 只接受 root 私有 Unix 流端点。取消会关闭普通连接与已劫持隧道，并等待复制工作者退出。
// 监听目录及实例锁由节点持有，端点校验失败时由调用方关闭监听器。
func (proxy *Server) Serve(parent context.Context, listener *net.UnixListener) (failure error) {
	if listener == nil || validateListener(listener) != nil || !proxy.started.CompareAndSwap(false, true) {
		return errors.New("电脑出网监听身份无效或重复启动")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	server := &http.Server{Handler: http.HandlerFunc(proxy.handle), ReadHeaderTimeout: proxy.options.HeaderTimeout, ReadTimeout: proxy.options.RequestTimeout, WriteTimeout: proxy.options.RequestTimeout, IdleTimeout: proxy.options.IdleTimeout, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }, ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			_, ok := connection.(*trackedConnection)
			return context.WithValue(ctx, authorizedKey{}, ok)
		}, ErrorLog: log.New(errorLog{proxy.logger.With("user_id", proxy.owner.UserID, "computer_id", proxy.owner.ComputerID, "generation", proxy.owner.Generation)}, "", 0)}
	// 保留基数，避免停止监听时仍在派发的 HTTP 处理器与 Wait 竞争。
	proxy.pending.Add(1)
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closed <- errors.Join(server.Close(), proxy.closeConnections()) })
	err := server.Serve(&authorizedListener{UnixListener: listener, proxy: proxy, ctx: ctx})
	if parent.Err() == nil {
		failure = errors.Join(failure, errors.New("电脑出网监听提前终止"))
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		failure = errors.Join(failure, errors.New("电脑出网服务失败"))
	}
	cancel()
	if stop() {
		failure = errors.Join(failure, server.Close(), proxy.closeConnections())
	} else {
		failure = errors.Join(failure, <-closed)
	}
	shutdown, finish := context.WithTimeout(context.WithoutCancel(parent), proxy.options.ShutdownTimeout)
	defer finish()
	if err := server.Shutdown(shutdown); err != nil {
		failure = errors.Join(failure, errors.New("电脑出网 HTTP 收尾超时"))
	}
	proxy.pending.Done()
	go func() { proxy.pending.Wait(); close(proxy.drained) }()
	select {
	case <-proxy.drained:
		return failure
	case <-shutdown.Done():
		return errors.Join(failure, errors.New("电脑出网隧道退出超时"))
	}
}

// Drain 在 Serve 返回后确认全部处理者已结束；退出超时不能被调用方当作可以释放实例锁的证据。
func (proxy *Server) Drain(ctx context.Context) error {
	if !proxy.started.Load() {
		return nil
	}
	select {
	case <-proxy.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type authorizedListener struct {
	*net.UnixListener
	proxy *Server
	ctx   context.Context
}

func (listener *authorizedListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		select {
		case listener.proxy.slots <- struct{}{}:
		default:
			if err := connection.Close(); err != nil {
				listener.proxy.logger.Warn("关闭超额出网连接失败", "user_id", listener.proxy.owner.UserID)
			}
			continue
		}
		peer, err := readPeer(connection)
		if err == nil && (peer.PID <= 1 || peer.UID != 0 || peer.GID != 0) {
			err = errors.New("电脑出网对端不是受信代理")
		}
		if err == nil {
			ctx, cancel := context.WithTimeout(listener.ctx, listener.proxy.options.ConnectTimeout)
			err = listener.proxy.check(ctx, peer)
			cancel()
		}
		if err != nil || listener.ctx.Err() != nil {
			if closeErr := connection.Close(); closeErr != nil {
				listener.proxy.logger.Warn("关闭未授权出网连接失败", "user_id", listener.proxy.owner.UserID)
			}
			<-listener.proxy.slots
			continue
		}
		tracked := &trackedConnection{Conn: connection, proxy: listener.proxy}
		listener.proxy.mu.Lock()
		closing := listener.proxy.closing
		if !closing {
			listener.proxy.connections[tracked] = struct{}{}
		}
		listener.proxy.mu.Unlock()
		if closing {
			if err := tracked.Close(); err != nil {
				listener.proxy.logger.Warn("关闭停服期间的出网连接失败", "user_id", listener.proxy.owner.UserID)
			}
			continue
		}
		return tracked, nil
	}
}

type trackedConnection struct {
	net.Conn
	proxy   *Server
	once    sync.Once
	failure error
}

func (connection *trackedConnection) Close() error {
	connection.once.Do(func() {
		connection.failure = connection.Conn.Close()
		connection.proxy.mu.Lock()
		delete(connection.proxy.connections, connection)
		connection.proxy.mu.Unlock()
		<-connection.proxy.slots
	})
	return connection.failure
}
func (connection *trackedConnection) CloseWrite() error {
	if half, ok := connection.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return errors.New("出网连接不支持半关闭")
}

func (proxy *Server) closeConnections() error {
	proxy.mu.Lock()
	proxy.closing = true
	connections := make([]net.Conn, 0, len(proxy.connections))
	for connection := range proxy.connections {
		connections = append(connections, connection)
	}
	proxy.mu.Unlock()
	return closeNetworkConnections(connections...)
}

func closeNetworkConnections(connections ...net.Conn) error {
	var failure error
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failure = errors.Join(failure, errors.New("关闭电脑出网连接失败"))
		}
	}
	return failure
}
