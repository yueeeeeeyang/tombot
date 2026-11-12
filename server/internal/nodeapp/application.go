package nodeapp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/processgroup"
)

var ErrProcessExitRequired = errors.New("节点资源尚未确认停止，必须退出进程并保留恢复记录")

type service struct {
	name string
	run  func(context.Context) error
}
type cleanupStep struct {
	name  string
	close func(context.Context) error
}

type Application struct {
	nodeID   string
	logger   *slog.Logger
	shutdown time.Duration
	services []service
	cleanup  []cleanupStep
	state    atomic.Int32
}

// New 依次建立真实磁盘后端、节点、恢复后的会话和控制端点；恢复未完成时不开放监听。
func New(parent context.Context, configuration Config, logger *slog.Logger) (application *Application, result error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || logger == nil {
		return nil, errors.New("宿主节点要求 Linux root 系统服务和业务日志")
	}
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, configuration.StartupTimeout.duration())
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	steps := []cleanupStep{}
	defer func() {
		if result != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), configuration.ShutdownTimeout.duration())
			defer stop()
			result = errors.Join(result, finish(cleanup, steps, logger, configuration.NodeID))
		}
	}()
	storage, err := dockernode.NewFileStorage(configuration.storageOptions())
	if err != nil {
		return nil, err
	}
	steps = append(steps, cleanupStep{"磁盘记录", func(context.Context) error { return storage.Close() }})
	groups, err := processgroup.New(configuration.Groups.options(), logger)
	if err != nil {
		return nil, err
	}
	node, err := dockernode.New(ctx, configuration.runtimeOptions(), storage, logger)
	if err != nil {
		return nil, err
	}
	steps = append([]cleanupStep{{"容器与出网", func(context.Context) error { return node.Close() }}}, steps...)
	sessions, err := dockernode.NewSessions(ctx, node, groups, configuration.Sessions.options(), logger)
	if err != nil {
		return nil, err
	}
	steps = append([]cleanupStep{{"执行会话", sessions.Shutdown}}, steps...)
	server, err := nodeapi.NewServer(nodeapi.Operations{Node: node, Computers: node, Sessions: sessions, Terminals: sessions, Desktops: sessions, Agents: sessions}, configuration.apiOptions(), logger)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listener, err := nodeapi.Listen(configuration.Socket, configuration.ClientGID)
	if err != nil {
		return nil, err
	}
	// 控制端点锁最后释放，避免旧资源仍在收尾时另一节点入口重新接单。
	steps = append(steps, cleanupStep{"控制端点", func(context.Context) error { return listener.Close() }})
	application = &Application{nodeID: configuration.NodeID, logger: logger, shutdown: configuration.ShutdownTimeout.duration(), cleanup: steps, services: []service{{"节点 API", func(ctx context.Context) error { return server.Serve(ctx, listener.UnixListener) }}, {"执行监督", sessions.Serve}, {"出网监督", node.Serve}}}
	return application, nil
}

// finish 在任何释放失败或超时后停止后续释放；未知活动资源不能被当作已停止。
func finish(ctx context.Context, steps []cleanupStep, logger *slog.Logger, nodeID string) error {
	if len(steps) == 0 {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		for _, step := range steps {
			if err := ctx.Err(); err != nil {
				done <- err
				return
			}
			if err := step.close(ctx); err != nil {
				logger.Error("节点资源收尾未确认", "node_id", nodeID, "component", step.name)
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			return errors.Join(ErrProcessExitRequired, err)
		}
		return nil
	case <-ctx.Done():
		return errors.Join(ErrProcessExitRequired, ctx.Err())
	}
}

// Close 仅用于尚未进入 Serve 的构造结果；运行中必须先取消 Serve 并由统一监督完成收尾。
func (application *Application) Close(ctx context.Context) error {
	if !application.state.CompareAndSwap(0, 2) {
		return errors.New("运行中的节点不能单独关闭资源")
	}
	err := finish(ctx, application.cleanup, application.logger, application.nodeID)
	if err != nil {
		application.state.Store(4)
	} else {
		application.state.Store(3)
	}
	return err
}

// Serve 同时监督控制 API、执行租约及出网，任一意外退出都会停止接单；ready 仅通知控制服务已启动。
func (application *Application) Serve(parent context.Context, ready func(context.Context) error) (failure error) {
	if !application.state.CompareAndSwap(0, 1) {
		return errors.New("节点服务不能重复启动")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, len(application.services))
	for _, component := range application.services {
		go func() { results <- outcome{component.name, component.run(ctx)} }()
	}
	if ready != nil && ctx.Err() == nil {
		failure = ready(ctx)
	}
	if failure == nil && ctx.Err() == nil {
		application.logger.Info("节点控制服务已启动，用户电脑仍需完整能力检查", "node_id", application.nodeID, "scope", "node_control")
	}
	received := 0
	unsafe := false
	accept := func(result outcome) {
		received++
		if errors.Is(result.err, nodeapi.ErrUnfinishedOperations) {
			unsafe = true
		}
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			application.logger.Error("节点服务退出异常", "node_id", application.nodeID, "component", result.name)
			failure = errors.Join(failure, result.err)
		}
	}
	if failure == nil {
		select {
		case <-parent.Done():
		case result := <-results:
			if parent.Err() == nil {
				failure = errors.New("节点监督组件提前退出")
			}
			accept(result)
		}
	}
	application.state.Store(2)
	cancel()
	shutdown, stop := context.WithTimeout(context.WithoutCancel(parent), application.shutdown)
	defer stop()
	for received < len(application.services) {
		select {
		case result := <-results:
			accept(result)
		case <-shutdown.Done():
			application.state.Store(4)
			return errors.Join(failure, ErrProcessExitRequired, shutdown.Err())
		}
	}
	if unsafe {
		application.state.Store(4)
		return errors.Join(failure, ErrProcessExitRequired)
	}
	if err := finish(shutdown, application.cleanup, application.logger, application.nodeID); err != nil {
		application.state.Store(4)
		return errors.Join(failure, err)
	}
	application.state.Store(3)
	return failure
}
