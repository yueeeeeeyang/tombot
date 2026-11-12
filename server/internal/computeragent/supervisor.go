package computeragent

import (
	"context"
	"errors"
	"time"
)

// supervise 独立等待控制服务与看门狗；致命故障不等待可能阻塞的刷盘或 RPC 收尾。
// 返回失败后调用进程必须立即退出，不能释放持久锁再继续运行另一个实例。
func supervise(ctx context.Context, timeout time.Duration, serve, watchdog func(context.Context) error, shutdown func(context.Context) error, closeResources func() error) error {
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	serverResult, watchdogResult := make(chan error, 1), make(chan error, 1)
	go func() { serverResult <- serve(running) }()
	go func() { watchdogResult <- watchdog(running) }()
	var cause error
	serverDone, watchdogDone := false, false
	select {
	case err := <-watchdogResult:
		watchdogDone = true
		if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
			return errors.New("电脑代理看门狗终止，必须退出容器")
		}
	case err := <-serverResult:
		serverDone = true
		if ctx.Err() == nil || !errors.Is(err, context.Canceled) {
			cause = errors.New("电脑代理控制服务终止")
		}
	case <-ctx.Done():
	}
	cancel()
	bounded, stop := context.WithTimeout(context.Background(), timeout)
	defer stop()
	cleaned := make(chan error, 1)
	go func() { cleaned <- shutdown(bounded) }()
	shutdownDone := false
	for !serverDone || !watchdogDone || !shutdownDone {
		select {
		case <-bounded.Done():
			return errors.Join(cause, errors.New("电脑代理停止超时，必须退出容器"))
		case err := <-serverResult:
			serverDone = true
			if err != nil && !errors.Is(err, context.Canceled) {
				cause = errors.Join(cause, errors.New("电脑代理控制服务收尾失败"))
			}
		case err := <-watchdogResult:
			watchdogDone = true
			if err == nil || !errors.Is(err, context.Canceled) {
				return errors.Join(cause, errors.New("电脑代理看门狗收尾失败，必须退出容器"))
			}
		case err := <-cleaned:
			shutdownDone = true
			if err != nil {
				return errors.Join(cause, errors.New("电脑代理执行环境收尾失败，必须退出容器"))
			}
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- closeResources() }()
	select {
	case err := <-closed:
		return errors.Join(cause, err)
	case <-bounded.Done():
		return errors.Join(cause, errors.New("电脑代理控制记录关闭超时，必须退出容器"))
	}
}
