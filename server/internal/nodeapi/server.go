package nodeapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type ServerOptions struct {
	NodeID                       string
	CallTimeout, ShutdownTimeout time.Duration
	MaximumConnections           int
}

var ErrUnfinishedOperations = errors.New("节点在途动作未确认收尾，进程必须退出")

// Validate 检查节点身份、调用与退出时限；请求方身份由 TLS 证书指纹在握手期确认。
func (options ServerOptions) Validate() error {
	if !validNode(options.NodeID) || !validTimeout(options.CallTimeout) || options.ShutdownTimeout < options.CallTimeout || options.ShutdownTimeout > 2*time.Minute || options.MaximumConnections < 1 || options.MaximumConnections > 1024 {
		return errors.New("电脑节点服务配置无效")
	}
	return nil
}

type Server struct {
	operations Operations
	options    ServerOptions
	logger     *slog.Logger
}

// NewServer 固定节点标识与调用预算，缺少任一电脑能力实现时拒绝构造。
func NewServer(operations Operations, options ServerOptions, logger *slog.Logger) (*Server, error) {
	if operations.Node == nil || operations.Computers == nil || operations.Sessions == nil || operations.Terminals == nil || operations.Desktops == nil || operations.Agents == nil || options.Validate() != nil || logger == nil {
		return nil, errors.New("电脑节点服务配置无效")
	}
	return &Server{operations: operations, options: options, logger: logger}, nil
}

// Serve 在已终止 TLS 的监听器上提供固定动作端点；连接数有上限，取消后等待在途动作收尾。
func (server *Server) Serve(ctx context.Context, listener net.Listener) (failure error) {
	if listener == nil {
		return errors.New("电脑节点监听器缺失")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	limited := newLimitListener(listener, server.options.MaximumConnections)
	httpServer := &http.Server{
		Handler:           http.HandlerFunc(server.serveAction),
		ReadHeaderTimeout: server.options.CallTimeout,
		ReadTimeout:       server.options.CallTimeout,
		WriteTimeout:      server.options.CallTimeout,
		IdleTimeout:       server.options.CallTimeout,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          nil,
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(stopped) })
	defer func() { stop() }()
	result := make(chan error, 1)
	go func() { result <- httpServer.Serve(limited) }()
	select {
	case <-ctx.Done():
	case err := <-result:
		if ctx.Err() == nil {
			failure = errors.New("电脑节点监听提前结束")
			if !errors.Is(err, http.ErrServerClosed) {
				failure = errors.New("电脑节点监听失败")
			}
		}
	}
	shutdown, finish := context.WithTimeout(context.WithoutCancel(ctx), server.options.ShutdownTimeout)
	defer finish()
	if err := httpServer.Shutdown(shutdown); err != nil {
		// 连接未在预算内收尾说明仍有在途动作，进程必须退出并由持久记录恢复。
		return errors.Join(failure, ErrUnfinishedOperations, err)
	}
	if err := limited.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		failure = errors.Join(failure, errors.New("关闭节点监听失败"))
	}
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			failure = errors.Join(failure, errors.New("电脑节点监听退出异常"))
		}
	default:
	}
	return failure
}

// serveAction 只接受一个固定动作；请求期限按收包时刻重建，传输耗时不延长任何授权。
func (server *Server) serveAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v2/actions" || r.URL.RawQuery != "" {
		writeFailure(w, http.StatusNotFound)
		return
	}
	encoded, err := io.ReadAll(http.MaxBytesReader(w, r.Body, agentchannel.MaximumBytes))
	if err != nil || len(encoded) == 0 {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	var content request
	if agentchannel.Decode(encoded, &content) != nil {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	if !validID(content.RequestID) || content.NodeID != server.options.NodeID {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	if content.RemainingMillis < 1 || content.RemainingMillis > server.options.CallTimeout.Milliseconds() {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	input, lifetime, err := decodeInput(server.options.NodeID, content.Action, content.Input)
	if err != nil {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	// 租约只能收紧：接收方按自己的时钟重算剩余时长，请求方无法借此延长执行权。
	var leaseDeadline time.Time
	if lifetime > 0 {
		if content.LeaseRemainMillis == nil || *content.LeaseRemainMillis < 1 || *content.LeaseRemainMillis > lifetime.Milliseconds() {
			writeFailure(w, http.StatusBadRequest)
			return
		}
		leaseDeadline = time.Now().Add(time.Duration(*content.LeaseRemainMillis) * time.Millisecond)
	} else if content.LeaseRemainMillis != nil {
		writeFailure(w, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(content.RemainingMillis)*time.Millisecond)
	defer cancel()
	if lifetime > 0 {
		ctx = computer.WithLeaseDeadline(ctx, leaseDeadline)
	}
	output, callErr := server.operations.invoke(ctx, content.Action, input)
	result := response{RequestID: content.RequestID, NodeID: server.options.NodeID, Action: content.Action}
	if callErr == nil {
		callErr = ctx.Err()
	}
	if callErr == nil {
		callErr = validateResult(content.Action, input, output)
	}
	if callErr == nil {
		result.Result, callErr = agentchannel.Encode(output)
	}
	if callErr != nil {
		result.Result = nil
		result.Failure = &computer.Failure{Code: "node_operation_unknown"}
		var classified *computer.Failure
		if errors.As(callErr, &classified) && codePattern.MatchString(classified.Code) {
			result.Failure = &computer.Failure{Code: classified.Code, IsPermanent: classified.IsPermanent}
		}
		server.logger.WarnContext(ctx, "电脑节点动作未确认成功", "node_id", server.options.NodeID, "request_id", content.RequestID, "action", content.Action)
	}
	body, err := agentchannel.Encode(result)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		server.logger.WarnContext(ctx, "电脑节点响应未发送完成", "node_id", server.options.NodeID, "request_id", content.RequestID)
	}
}

// writeFailure 只返回状态码；拒绝原因不回给调用方，避免暴露内部判定顺序。
func writeFailure(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}

// limitListener 限制同时存活的连接；超出的连接立即关闭，不排队等待。
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(listener net.Listener, maximum int) *limitListener {
	return &limitListener{Listener: listener, sem: make(chan struct{}, maximum)}
}

func (listener *limitListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case listener.sem <- struct{}{}:
			return &limitConn{Conn: connection, release: func() { <-listener.sem }}, nil
		default:
			if err := connection.Close(); err != nil {
				continue
			}
		}
	}
}

type limitConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (connection *limitConn) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(connection.release)
	return err
}
