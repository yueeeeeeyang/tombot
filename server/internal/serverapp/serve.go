package serverapp

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
)

type httpLog struct{ logger *slog.Logger }

func (writer httpLog) Write(message []byte) (int, error) {
	writer.logger.Warn("HTTPS 连接处理失败", "diagnostic_bytes", len(message))
	return len(message), nil
}

// Serve 将 API、开户工作池和执行 Worker 作为同一进程监督；任一后台提前结束均停止接单。
func (application *Application) Serve(parent context.Context, listener net.Listener) (failure error) {
	if listener == nil || !application.state.CompareAndSwap(stateNew, stateRunning) {
		return errors.New("服务不能重复启动或缺少监听器")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	options := application.configuration.HTTP
	server := &http.Server{Handler: application.handler, TLSConfig: application.tls, ReadHeaderTimeout: options.ReadHeaderTimeout.Value(), ReadTimeout: options.ReadTimeout.Value(), WriteTimeout: options.WriteTimeout.Value(), IdleTimeout: options.IdleTimeout.Value(), MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }, ErrorLog: log.New(httpLog{application.logger}, "", 0)}
	results := make(chan error, len(application.background)+1)
	for _, run := range application.background {
		go func() {
			err := run(ctx)
			if err == nil && ctx.Err() == nil {
				err = errors.New("后台服务提前结束")
			}
			results <- err
		}()
	}
	go func() { results <- server.ServeTLS(listener, "", "") }()
	application.logger.InfoContext(ctx, "多用户服务开始接收请求", "address", listener.Addr().String())
	received := 0
	select {
	case <-parent.Done():
	case err := <-results:
		received++
		if parent.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
			break
		}
		failure = err
		if err == nil {
			failure = errors.New("服务监听提前结束")
		}
	}
	// 停单与取消先于资源释放，Worker 仍可用独立收尾上下文记录未知结果并清理所属会话。
	application.state.Store(stateStopping)
	cancel()
	shutdown, stop := context.WithTimeout(context.WithoutCancel(parent), options.ShutdownTimeout.Value())
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		failure = errors.Join(failure, errors.New("HTTPS 服务未能按时停止"), server.Close())
	}
	for received < len(application.background)+1 {
		select {
		case err := <-results:
			received++
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				failure = errors.Join(failure, err)
			}
		case <-shutdown.Done():
			return errors.Join(failure, errors.New("后台服务退出超时，进程必须结束"))
		}
	}
	application.state.Store(stateStopped)
	return failure
}
