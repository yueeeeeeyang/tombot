package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// BrowserOptions 只约束升级时旧共享服务的退出与回收。
type BrowserOptions struct {
	CallTimeout     time.Duration
	CleanupTimeout  time.Duration
	ShutdownTimeout time.Duration
	Groups          processgroup.Options
}

func (options BrowserOptions) Validate() error {
	if options.Groups.Validate() != nil || options.CallTimeout < time.Second || options.CleanupTimeout <= options.CallTimeout+options.Groups.GracePeriod+options.Groups.PollInterval || options.CleanupTimeout > time.Minute || options.ShutdownTimeout < options.CleanupTimeout || options.ShutdownTimeout > 5*time.Minute {
		return errors.New("节点共享浏览器预算无效")
	}
	return nil
}

type computerBrowsers interface {
	retire(context.Context, Resource) error
	shutdown(context.Context) error
}

// nodeBrowsers 运行操作由用户生命周期独占锁串行；恢复和停服逐用户取锁，不恢复未知启动。
type nodeBrowsers struct {
	node    *Runtime
	store   browserStore
	groups  processgroup.ServiceGroups
	options BrowserOptions
	logger  *slog.Logger
	locate  func(context.Context, string, int) (processgroup.ContainerReference, error)
	invoke  func(context.Context, Resource, computeragent.BrowserGrant, string, []*os.File) (computeragent.BrowserObservation, error)
}

func newBrowsers(node *Runtime, logger *slog.Logger) (*nodeBrowsers, error) {
	groups, err := processgroup.NewServices(node.options.Browser.Groups, logger)
	if err != nil {
		return nil, err
	}
	browsers := &nodeBrowsers{node: node, store: node.journal, groups: groups, options: node.options.Browser, logger: logger, locate: processgroup.LocateContainer}
	browsers.invoke = browsers.call
	return browsers, nil
}

func (browsers *nodeBrowsers) previous(resource Resource) (*browserRecord, error) {
	state, err := browsers.store.readBrowser(resource.Specification.UserID)
	if err != nil || state == nil {
		return state, err
	}
	previous, current := state.Specification, resource.Specification
	if state.validate(current.UserID) != nil || !sameComputerIdentity(previous, current) || previous.Generation > current.Generation || previous.Generation == current.Generation && !immutableEqual(previous, current) {
		return nil, failure("browser_instance_mismatch")
	}
	return state, nil
}

// retire 只回收升级前已持久登记的原实例。
func (browsers *nodeBrowsers) retire(ctx context.Context, resource Resource) error {
	state, err := browsers.previous(resource)
	if err != nil || state == nil || state.Phase == "closed" {
		return err
	}
	return browsers.close(ctx, state)
}

// close 先封闭宿主意图，再向代理安装关闭屏障；回包未知时隔离容器，不能只清空资源组后允许迟到启动。
func (browsers *nodeBrowsers) close(ctx context.Context, state *browserRecord) (result error) {
	if state.Phase == "closed" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, browsers.options.CleanupTimeout)
	defer cancel()
	resource := browsers.node.resource(state.Specification)
	state.Phase = "closing"
	saveErr := browsers.store.saveBrowser(state)
	defer func() {
		if result != nil {
			browsers.logger.Error("共享浏览器关闭未完整确认", "user_id", state.Specification.UserID, "computer_id", state.Specification.ComputerID, "generation", state.Specification.Generation, "service_id", state.Grant.Request.Group.ID, "reason", result.Error())
		}
	}()
	current, err := browsers.current(ctx, resource, state.Grant)
	if err != nil {
		return errors.Join(saveErr, err)
	}
	closure := "isolated"
	var callErr error
	if current != nil && current.State.Running {
		call, stop := context.WithTimeout(ctx, browsers.options.CallTimeout)
		observed, err := browsers.invoke(call, resource, state.Grant, "close", nil)
		stop()
		if err == nil && observed.Grant == state.Grant && observed.Phase == "closed" {
			closure = "agent"
		} else {
			callErr = errors.Join(failure("browser_close_unconfirmed"), err)
			if err := browsers.isolate(ctx, resource, state.Grant); err != nil {
				return errors.Join(saveErr, callErr, err)
			}
			browsers.logger.WarnContext(ctx, "共享浏览器回包未知，原容器已隔离", "user_id", state.Specification.UserID, "computer_id", state.Specification.ComputerID, "generation", state.Specification.Generation)
		}
	}
	// 即使刷盘失败也尝试回收已知资源，但不以回收成功掩盖未提交的关闭屏障。
	drainErr := browsers.groups.Drain(ctx, state.Grant.Request.Group)
	if err := errors.Join(saveErr, drainErr, ctx.Err()); err != nil {
		return errors.Join(callErr, err)
	}
	state.Phase, state.Closure = "closed", closure
	return errors.Join(callErr, browsers.store.saveBrowser(state))
}

// current 只识别原容器；名称、配置或原生资源组变化均拒绝，不能停止同名替代实例。
func (browsers *nodeBrowsers) current(ctx context.Context, resource Resource, grant computeragent.BrowserGrant) (*container, error) {
	current, err := browsers.node.engine.inspectContainer(ctx, grant.Request.Group.Container.ContainerID)
	if errors.Is(err, errMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current.ID != grant.Request.Group.Container.ContainerID || current.Name != "/"+resource.Name || !sameLabels(current.Config.Labels, containerLabels(resource)) {
		return nil, failure("browser_container_changed")
	}
	if err := browsers.node.validateContainer(resource, current); err != nil {
		return nil, err
	}
	if current.State.Running {
		actual, err := browsers.node.serviceParent(ctx, resource, current.ID, browsers.locate)
		if err != nil || actual != grant.Request.Group.Container {
			return nil, errors.Join(failure("browser_parent_changed"), err)
		}
	}
	return &current, ctx.Err()
}

func (browsers *nodeBrowsers) isolate(ctx context.Context, resource Resource, grant computeragent.BrowserGrant) error {
	current, err := browsers.current(ctx, resource, grant)
	if err != nil || current == nil || !current.State.Running {
		return err
	}
	stopErr := browsers.node.engine.stopContainer(ctx, current.ID)
	current, inspectErr := browsers.current(ctx, resource, grant)
	if inspectErr != nil || current != nil && current.State.Running {
		return errors.Join(failure("browser_isolation_unconfirmed"), stopErr, inspectErr)
	}
	// 停止回包丢失时以实际停止状态为准；后续仍须核验原 cgroup 已排空。
	return nil
}

func (browsers *nodeBrowsers) recover(ctx context.Context) error {
	return browsers.store.visitBrowsers(ctx, func(userID string) (result error) {
		release, err := browsers.node.journal.lock(ctx, userID)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, release()) }()
		state, err := browsers.node.journal.read(userID)
		if err != nil {
			return err
		}
		if state == nil || state.Specification.NodeID != browsers.node.options.NodeID {
			return failure("browser_lifecycle_missing")
		}
		return browsers.retire(ctx, browsers.node.resource(state.Specification))
	})
}

// shutdown 在外层停止接单和等待在途调用后执行，不自动重建、重发或恢复任何浏览器。
func (browsers *nodeBrowsers) shutdown(ctx context.Context) error { return browsers.recover(ctx) }

func (browsers *nodeBrowsers) call(ctx context.Context, resource Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
	if action != "close" || len(files) != 0 {
		return computeragent.BrowserObservation{}, failure("browser_action_invalid")
	}
	client, err := browsers.node.serviceClient(ctx, resource, grant.Request.Group.Container, browsers.locate, browsers.options.CallTimeout)
	if err != nil {
		return computeragent.BrowserObservation{}, err
	}
	return client.CloseBrowser(ctx, grant)
}
