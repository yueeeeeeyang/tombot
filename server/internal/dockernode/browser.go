package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

// BrowserOptions 分别限制单次调用、原实例回收及整个节点停服，不从用户请求读取预算。
type BrowserOptions struct {
	CallTimeout     time.Duration
	CleanupTimeout  time.Duration
	ShutdownTimeout time.Duration
	Groups          processgroup.Options
	Limits          computeragent.BrowserLimits
}

func (options BrowserOptions) Validate() error {
	if options.Groups.Validate() != nil || options.Limits.Validate() != nil || options.CallTimeout < time.Second || options.CallTimeout > taskprocess.BrowserMaximumStartTime || options.CleanupTimeout <= options.CallTimeout+options.Groups.GracePeriod+options.Groups.PollInterval || options.CleanupTimeout > time.Minute || options.ShutdownTimeout < options.CleanupTimeout || options.ShutdownTimeout > 5*time.Minute {
		return errors.New("节点共享浏览器预算无效")
	}
	return nil
}

type computerBrowsers interface {
	ensure(context.Context, Resource, container) error
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
	stamp   func(time.Time) (agentchannel.Deadline, error)
	invoke  func(context.Context, Resource, computeragent.BrowserGrant, string, []*os.File) (computeragent.BrowserObservation, error)
}

func newBrowsers(node *Runtime, logger *slog.Logger) (*nodeBrowsers, error) {
	groups, err := processgroup.NewServices(node.options.Browser.Groups, logger)
	if err != nil {
		return nil, err
	}
	browsers := &nodeBrowsers{node: node, store: node.journal, groups: groups, options: node.options.Browser, logger: logger, locate: processgroup.LocateContainer, stamp: agentchannel.FromDeadline}
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

// ensure 只有显式对账能够创建新授权；活动服务只查询原授权，查询失败封闭原实例并返回失败。
func (browsers *nodeBrowsers) ensure(ctx context.Context, resource Resource, current container) (result error) {
	ctx, cancel := context.WithTimeout(ctx, browsers.options.CallTimeout)
	defer cancel()
	previous, err := browsers.previous(resource)
	if err != nil {
		return err
	}
	parent, err := browsers.node.serviceParent(ctx, resource, current.ID, browsers.locate)
	if err != nil {
		return err
	}
	if previous != nil && previous.Phase != "closed" {
		if previous.Phase == "active" && previous.Grant.Identity == browsers.node.agentConfiguration(resource).Identity && previous.Grant.Request.Group.Container == parent {
			observed, err := browsers.invoke(ctx, resource, previous.Grant, "status", nil)
			if err == nil && observed.Grant == previous.Grant && observed.Phase == "active" {
				actual, verifyErr := browsers.node.serviceParent(ctx, resource, current.ID, browsers.locate)
				if verifyErr == nil && actual == parent {
					return ctx.Err()
				}
				err = errors.Join(failure("browser_parent_changed"), verifyErr)
			}
			return errors.Join(failure("browser_status_unconfirmed"), err, browsers.close(context.WithoutCancel(ctx), previous))
		}
		return errors.Join(failure("browser_previous_unconfirmed"), browsers.close(context.WithoutCancel(ctx), previous))
	}
	revision := int64(1)
	if previous != nil {
		if previous.Grant.Revision == math.MaxInt64 {
			return failure("browser_revision_exhausted")
		}
		revision = previous.Grant.Revision + 1
	}
	deadline, _ := ctx.Deadline()
	stamp, err := browsers.stamp(deadline)
	if err != nil {
		return err
	}
	grant := computeragent.BrowserGrant{Identity: browsers.node.agentConfiguration(resource).Identity, Revision: revision, Request: taskprocess.BrowserRequest{Version: 1, Group: processgroup.ServiceReference{Container: parent, ID: ids.New(), Kind: processgroup.BrowserService}, Deadline: stamp}, Limits: browsers.options.Limits}
	state := &browserRecord{Version: 1, Specification: resource.Specification, Grant: grant, Phase: "prepared"}
	if err := browsers.store.saveBrowser(state); err != nil {
		return err
	}
	// Create、传递入口及回包都可能结果未知；后续失败只关闭原授权，不创建替代实例。
	defer func() {
		if result != nil {
			result = errors.Join(result, browsers.close(context.WithoutCancel(ctx), state))
		}
	}()
	if err := browsers.groups.Create(ctx, grant.Request.Group); err != nil {
		return err
	}
	files, err := browsers.groups.Controls(ctx, grant.Request.Group)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, agentchannel.CloseFiles(files)) }()
	state.Phase = "dispatched"
	if err := browsers.store.saveBrowser(state); err != nil {
		return err
	}
	observed, err := browsers.invoke(ctx, resource, grant, "start", files)
	if err != nil || observed.Grant != grant || observed.Phase != "active" {
		return errors.Join(failure("browser_start_unconfirmed"), err)
	}
	actual, err := browsers.node.serviceParent(ctx, resource, current.ID, browsers.locate)
	if err != nil || actual != parent {
		return errors.Join(failure("browser_parent_changed"), err)
	}
	state.Phase = "active"
	if err := browsers.store.saveBrowser(state); err != nil {
		return err
	}
	browsers.logger.InfoContext(ctx, "用户共享浏览器已确认启动", "user_id", grant.Identity.UserID, "computer_id", grant.Identity.ComputerID, "generation", grant.Identity.Generation, "service_id", grant.Request.Group.ID, "revision", grant.Revision)
	return ctx.Err()
}

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
	if current.ID != grant.Request.Group.Container.ContainerID || current.Name != "/"+resource.Name || !sameLabels(current.Config.Labels, resource.Labels) {
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
	client, err := browsers.node.serviceClient(ctx, resource, grant.Request.Group.Container, browsers.locate, browsers.options.CallTimeout)
	if err != nil {
		return computeragent.BrowserObservation{}, err
	}
	switch action {
	case "start":
		return client.StartBrowser(ctx, grant, files)
	case "status":
		return client.BrowserStatus(ctx, grant)
	case "close":
		return client.CloseBrowser(ctx, grant)
	default:
		return computeragent.BrowserObservation{}, failure("browser_action_invalid")
	}
}
