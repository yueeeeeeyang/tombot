package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/egressproxy"
	"io.yueyang/tom/internal/processgroup"
)

// computerEgress 由生命周期持有用户独占锁后调用；不持有此锁处理网络流量。
type computerEgress interface {
	ensure(context.Context, Resource, container) error
	stop(context.Context, Resource) error
	shutdown(context.Context) error
	wait(context.Context) error
}

type egressInstance interface {
	serve(context.Context) error
	close() error
	check() error
}

type egressEntry struct {
	resource Resource
	parent   processgroup.ContainerReference
	cancel   context.CancelFunc
	done     chan struct{}
	cleanup  error
	instance egressInstance
}

type egressController struct {
	node    *Runtime
	logger  *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	locate  func(context.Context, string, int) (processgroup.ContainerReference, error)
	create  func(context.Context, Resource, processgroup.ContainerReference) (egressInstance, error)
	clear   func(Resource) error
	mu      sync.Mutex
	closed  bool
	fault   error
	entries map[string]*egressEntry
	pending sync.WaitGroup
}

func newEgress(node *Runtime, logger *slog.Logger) *egressController {
	ctx, cancel := context.WithCancel(context.Background())
	controller := &egressController{node: node, logger: logger, ctx: ctx, cancel: cancel, locate: processgroup.LocateContainer, entries: map[string]*egressEntry{}}
	controller.create, controller.clear = controller.listen, controller.clearEndpoint
	return controller
}

func (controller *egressController) begin() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closed {
		return failure("egress_controller_closed")
	}
	controller.pending.Add(1)
	return nil
}

func (controller *egressController) ensure(ctx context.Context, resource Resource, current container) error {
	if err := controller.begin(); err != nil {
		return err
	}
	defer controller.pending.Done()
	if err := ctx.Err(); err != nil {
		return err
	}
	if controller.ctx.Err() != nil || !current.State.Running || current.State.Dead {
		return failure("egress_instance_unavailable")
	}
	parent, err := controller.locate(ctx, current.ID, current.State.Pid)
	if err != nil {
		return err
	}
	if parent.Validate() != nil || parent.ContainerID != current.ID {
		return failure("egress_parent_invalid")
	}
	controller.mu.Lock()
	entry := controller.entries[resource.Name]
	if entry != nil {
		controller.mu.Unlock()
		if !immutableEqual(entry.resource.Specification, resource.Specification) || entry.parent != parent {
			return failure("egress_instance_changed")
		}
		select {
		case <-entry.done:
			return failure("egress_instance_stopped")
		default:
			if err := entry.instance.check(); err != nil {
				entry.cancel()
				controller.logger.Warn("电脑出网端点漂移，已封闭连接", "user_id", resource.Specification.UserID, "computer_id", resource.Specification.ComputerID)
				return err
			}
			return nil
		}
	}
	if controller.closed || controller.ctx.Err() != nil || len(controller.entries) >= controller.node.options.MaximumEgressComputers {
		controller.mu.Unlock()
		return failure("egress_capacity_or_stopping")
	}
	running, cancel := context.WithCancel(controller.ctx)
	entry = &egressEntry{resource: resource, parent: parent, cancel: cancel, done: make(chan struct{})}
	controller.entries[resource.Name] = entry
	controller.mu.Unlock()
	instance, err := controller.create(ctx, resource, parent)
	if err == nil && instance == nil {
		err = errors.New("电脑出网实例未创建")
	}
	if err != nil {
		if instance != nil {
			err = errors.Join(err, instance.close())
		}
		cancel()
		controller.mu.Lock()
		delete(controller.entries, resource.Name)
		controller.mu.Unlock()
		close(entry.done)
		return err
	}
	entry.instance = instance
	// 建立端点使用请求期限；端点寿命由节点拥有，不能在开户请求返回时一并取消。
	go func() {
		err := instance.serve(running)
		unexpected := running.Err() == nil
		entry.cleanup = instance.close()
		if unexpected || err != nil || entry.cleanup != nil {
			controller.mu.Lock()
			controller.fault = errors.Join(controller.fault, errors.New("电脑出网服务异常结束"))
			controller.mu.Unlock()
			controller.logger.Error("电脑出网服务异常结束，节点出网已封闭", "user_id", resource.Specification.UserID, "computer_id", resource.Specification.ComputerID, "generation", resource.Specification.Generation)
			controller.cancel()
		}
		close(entry.done)
	}()
	if err := ctx.Err(); err != nil {
		// 端点已创建的结果由后续 Inspect 对账，不因请求失联删除仍可能使用的实例。
		return err
	}
	if running.Err() != nil {
		return failure("egress_instance_unavailable")
	}
	return nil
}

func (controller *egressController) stop(ctx context.Context, resource Resource) error {
	if err := controller.begin(); err != nil {
		return err
	}
	defer controller.pending.Done()
	controller.mu.Lock()
	entry := controller.entries[resource.Name]
	controller.mu.Unlock()
	if entry == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return controller.clear(resource)
	}
	if !immutableEqual(entry.resource.Specification, resource.Specification) {
		return failure("egress_instance_changed")
	}
	entry.cancel()
	select {
	case <-entry.done:
		if entry.cleanup != nil {
			return entry.cleanup
		}
		controller.mu.Lock()
		delete(controller.entries, resource.Name)
		controller.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown 先封闭创建并取消所有端点，确认在途创建、清理与连接工作者结束后才能释放节点目录。
func (controller *egressController) shutdown(ctx context.Context) error {
	controller.mu.Lock()
	controller.closed = true
	controller.mu.Unlock()
	controller.cancel()
	done := make(chan struct{})
	go func() { controller.pending.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	controller.mu.Lock()
	entries := make([]*egressEntry, 0, len(controller.entries))
	for _, entry := range controller.entries {
		entries = append(entries, entry)
	}
	controller.mu.Unlock()
	var result error
	for _, entry := range entries {
		select {
		case <-entry.done:
			result = errors.Join(result, entry.cleanup)
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

func (controller *egressController) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-controller.ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), controller.node.options.Egress.ShutdownTimeout)
	defer cancel()
	err := controller.shutdown(stop)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return errors.Join(controller.fault, err)
}

type proxyInstance struct {
	server   *egressproxy.Server
	listener *egressproxy.Listener
	shutdown time.Duration
}

func (instance *proxyInstance) serve(ctx context.Context) error {
	return instance.server.Serve(ctx, instance.listener.UnixListener)
}
func (instance *proxyInstance) check() error { return instance.listener.Check() }
func (instance *proxyInstance) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), instance.shutdown)
	defer cancel()
	if err := instance.server.Drain(ctx); err != nil {
		// 旧工作者未确认结束时保留锁；节点启动/退出报告失败，由进程退出释放所有句柄。
		return errors.New("电脑出网工作者尚未退出，端点锁已保留")
	}
	return instance.listener.Close()
}

func (controller *egressController) listen(ctx context.Context, resource Resource, parent processgroup.ContainerReference) (_ egressInstance, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := controller.node.agentDirectory(resource, false)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, directory.root.Close()) }()
	owner := egressproxy.Owner{UserID: resource.Specification.UserID, ComputerID: resource.Specification.ComputerID, ContainerID: parent.ContainerID, Generation: resource.Specification.Generation}
	server, err := egressproxy.New(controller.node.options.Egress, owner, func(ctx context.Context, peer egressproxy.Peer) error {
		return controller.checkPeer(ctx, resource, parent, peer)
	}, controller.logger)
	if err != nil {
		return nil, err
	}
	listener, err := egressproxy.Listen(filepath.Join(resource.ControlPath, computeragent.EgressSocketName))
	if err != nil {
		return nil, err
	}
	return &proxyInstance{server: server, listener: listener, shutdown: controller.node.options.Egress.ShutdownTimeout}, nil
}

func (controller *egressController) checkPeer(ctx context.Context, resource Resource, expected processgroup.ContainerReference, peer egressproxy.Peer) error {
	if peer.PID <= 1 || peer.UID != 0 || peer.GID != 0 || controller.ctx.Err() != nil {
		return failure("egress_peer_invalid")
	}
	current, err := controller.node.engine.inspectContainer(ctx, expected.ContainerID)
	if err != nil {
		return err
	}
	if current.ID != expected.ContainerID || current.Name != "/"+resource.Name || !sameLabels(current.Config.Labels, resource.Labels) || !current.State.Running || current.State.Dead {
		return failure("egress_container_changed")
	}
	if err := controller.node.validateContainer(resource, current); err != nil {
		return err
	}
	for _, pid := range []int{current.State.Pid, int(peer.PID)} {
		actual, err := controller.locate(ctx, expected.ContainerID, pid)
		if err != nil {
			return err
		}
		if actual != expected {
			return failure("egress_peer_container_mismatch")
		}
	}
	return ctx.Err()
}

func (controller *egressController) clearEndpoint(resource Resource) (result error) {
	_, err := controller.node.journal.root.Lstat(filepath.Base(resource.ControlPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := controller.node.agentDirectory(resource, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.root.Close()) }()
	// 持有稳定锁才能清理崩溃遗留端点；存在活监听或未知文件时拒绝接管。
	listener, err := egressproxy.Listen(filepath.Join(resource.ControlPath, computeragent.EgressSocketName))
	if err != nil {
		return err
	}
	return listener.Close()
}
