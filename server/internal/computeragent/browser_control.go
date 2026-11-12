package computeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

// BrowserLimits 只接受受信节点提供的运行预算，不包含网址、程序、环境或 profile 路径。
type BrowserLimits struct {
	Width                int `json:"width"`
	Height               int `json:"height"`
	MaximumWindows       int `json:"maximum_windows"`
	MaximumPerSession    int `json:"maximum_per_session"`
	MaximumWindowRecords int `json:"maximum_window_records"`
	MaximumEvents        int `json:"maximum_events"`
	CallTimeoutMS        int `json:"call_timeout_ms"`
	DialogTimeoutMS      int `json:"dialog_timeout_ms"`
	ShutdownTimeoutMS    int `json:"shutdown_timeout_ms"`
}

func (limits BrowserLimits) options() sharedbrowser.Options {
	return sharedbrowser.Options{Width: limits.Width, Height: limits.Height, MaximumWindows: limits.MaximumWindows, MaximumPerSession: limits.MaximumPerSession, MaximumWindowRecords: limits.MaximumWindowRecords, MaximumEvents: limits.MaximumEvents, CallTimeout: time.Duration(limits.CallTimeoutMS) * time.Millisecond, DialogTimeout: time.Duration(limits.DialogTimeoutMS) * time.Millisecond, ShutdownTimeout: time.Duration(limits.ShutdownTimeoutMS) * time.Millisecond}
}

// Validate 在时长换算前拒绝溢出，再复用共享浏览器的容量规则。
func (limits BrowserLimits) Validate() error {
	if limits.CallTimeoutMS < 100 || limits.CallTimeoutMS > 30000 || limits.DialogTimeoutMS < 1000 || limits.DialogTimeoutMS > 600000 || limits.ShutdownTimeoutMS < 1000 || limits.ShutdownTimeoutMS > 30000 {
		return errors.New("共享浏览器服务时限无效")
	}
	return limits.options().ValidateLimits()
}

// BrowserGrant 的版本跨容器代次递增；服务实例、启动期限与预算共同组成不可改绑的授权。
type BrowserGrant struct {
	Identity Identity                   `json:"identity"`
	Revision int64                      `json:"revision"`
	Request  taskprocess.BrowserRequest `json:"request"`
	Limits   BrowserLimits              `json:"limits"`
}

// Validate 只校验不可变结构；查询和关闭允许引用已过期的启动期限，启动另行检查原始时钟。
func (grant BrowserGrant) Validate() error {
	if grant.Identity.validate() != nil || grant.Revision < 1 || grant.Request.Validate() != nil || grant.Limits.Validate() != nil {
		return errors.New("共享浏览器服务授权无效")
	}
	return nil
}

// BrowserSnapshot 保存最后消费版本与关闭屏障，不从 active 记录恢复浏览器执行。
type BrowserSnapshot struct {
	Version int          `json:"version"`
	Grant   BrowserGrant `json:"grant"`
	Phase   string       `json:"phase"`
}

// Validate 核验完整持久授权与阶段，供代理恢复和宿主目录回收共用。
func (snapshot BrowserSnapshot) Validate() error {
	if snapshot.Version != 1 || snapshot.Grant.Validate() != nil || snapshot.Phase != "consumed" && snapshot.Phase != "active" && snapshot.Phase != "closed" {
		return errors.New("共享浏览器持久服务记录无效")
	}
	return nil
}

// BrowserStore 必须原子保存并刷盘；提交错误可能代表结果未知，监督器必须封闭实例。
type BrowserStore interface {
	LoadBrowser(context.Context) (*BrowserSnapshot, error)
	SaveBrowser(context.Context, BrowserSnapshot) error
}

// BrowserObservation 仅确认该实例的代理状态；active 不代表完整电脑或窗口客户端已验收。
type BrowserObservation struct {
	Grant BrowserGrant `json:"grant"`
	Phase string       `json:"phase"`
}

type browserInstance struct {
	grant    BrowserGrant
	scope    BrowserScope
	service  BrowserService
	stopping atomic.Bool
	watching bool
	watched  chan struct{}
}

// BrowserController 串行管理一个用户级 Chrome；耗时操作不持有 Bot 桌面锁，失控时退出整个容器。
type BrowserController struct {
	identity      Identity
	store         BrowserStore
	factory       BrowserFactory
	options       Options
	authorize     sharedbrowser.Authorizer
	logger        *slog.Logger
	onFatal       func()
	gate          chan struct{}
	mu            sync.Mutex
	stopped       bool
	pending       sync.WaitGroup
	state         *BrowserSnapshot
	current       *browserInstance
	available     atomic.Pointer[browserInstance]
	lifetime      context.Context
	stop          context.CancelFunc
	fatal         chan struct{}
	fatalOnce     sync.Once
	shutdownOnce  sync.Once
	shutdownDone  chan struct{}
	shutdownError error
}

// NewBrowserController 在监听前核验持久身份；同代次未闭合记录阻止启动，更高代次必须由节点先隔离旧容器。
func NewBrowserController(ctx context.Context, identity Identity, store BrowserStore, factory BrowserFactory, options Options, authorize sharedbrowser.Authorizer, logger *slog.Logger, onFatal func()) (*BrowserController, error) {
	if ctx == nil || identity.validate() != nil || store == nil || factory == nil || options.validate() != nil || authorize == nil || logger == nil || onFatal == nil {
		return nil, errors.New("共享浏览器监督配置无效")
	}
	bounded, cancel := context.WithTimeout(ctx, options.CallTimeout)
	defer cancel()
	state, err := store.LoadBrowser(bounded)
	if err != nil {
		return nil, err
	}
	if state != nil {
		previous := state.Grant.Identity
		if state.Validate() != nil || previous.UserID != identity.UserID || previous.ComputerID != identity.ComputerID || previous.NodeID != identity.NodeID || previous.Generation > identity.Generation {
			return nil, errors.New("共享浏览器持久身份不符")
		}
		if state.Phase != "closed" {
			if previous.Generation == identity.Generation {
				return nil, errors.New("共享浏览器旧实例未确认关闭，需要隔离当前容器")
			}
			closed := *state
			closed.Phase = "closed"
			if err := store.SaveBrowser(bounded, closed); err != nil {
				return nil, err
			}
			state = &closed
		}
	}
	if bounded.Err() != nil {
		return nil, bounded.Err()
	}
	lifetime, stop := context.WithCancel(context.Background())
	return &BrowserController{identity: identity, store: store, factory: factory, options: options, authorize: authorize, logger: logger, onFatal: onFatal, gate: make(chan struct{}, 1), state: state, lifetime: lifetime, stop: stop, fatal: make(chan struct{}), shutdownDone: make(chan struct{})}, nil
}

func (controller *BrowserController) fail() {
	controller.fatalOnce.Do(func() {
		controller.available.Store(nil)
		close(controller.fatal)
		controller.stop()
		controller.logger.Error("共享浏览器失去可靠控制，必须退出容器", "user_id", controller.identity.UserID, "computer_id", controller.identity.ComputerID, "generation", controller.identity.Generation)
		controller.onFatal()
	})
}

type browserOperation struct {
	kind  string
	grant BrowserGrant
	files []*os.File
}

// Start 接管三个传入入口，已消费请求不重放；回包未知时只允许查询或关闭同一授权。
func (controller *BrowserController) Start(ctx context.Context, grant BrowserGrant, files []*os.File) (BrowserObservation, error) {
	deadline, err := grant.Request.Deadline.Local(taskprocess.BrowserMaximumStartTime)
	if err != nil {
		return BrowserObservation{}, errors.Join(err, agentchannel.CloseFiles(files))
	}
	return controller.startAt(ctx, grant, files, deadline)
}

func (controller *BrowserController) startAt(ctx context.Context, grant BrowserGrant, files []*os.File, deadline time.Time) (BrowserObservation, error) {
	return controller.execute(ctx, browserOperation{kind: "start", grant: grant, files: files}, deadline)
}

// Status 只查询完全相同授权的已知状态，不启动、重试或延长服务。
func (controller *BrowserController) Status(ctx context.Context, grant BrowserGrant) (BrowserObservation, error) {
	return controller.execute(ctx, browserOperation{kind: "status", grant: grant}, time.Now().Add(controller.options.CallTimeout))
}

// Close 可在启动请求抵达前持久关闭同一授权，阻止迟到启动；旧授权不能影响新实例。
func (controller *BrowserController) Close(ctx context.Context, grant BrowserGrant) (BrowserObservation, error) {
	return controller.execute(ctx, browserOperation{kind: "close", grant: grant}, time.Now().Add(controller.options.CallTimeout))
}

func (controller *BrowserController) execute(ctx context.Context, operation browserOperation, deadline time.Time) (observation BrowserObservation, failure error) {
	defer func() {
		failure = errors.Join(failure, agentchannel.CloseFiles(operation.files))
		if failure != nil {
			observation = BrowserObservation{}
		}
	}()
	grant := operation.grant
	if ctx == nil || grant.Validate() != nil || grant.Identity != controller.identity || operation.kind == "start" && len(operation.files) != 3 || !time.Now().Before(deadline) {
		return observation, errors.New("共享浏览器控制请求身份、入口或期限无效")
	}
	limits := grant.Limits.options()
	if limits.CallTimeout > controller.options.CallTimeout || limits.ShutdownTimeout > controller.options.CleanupTimeout {
		return observation, errors.New("共享浏览器服务预算超过代理部署限制")
	}
	controller.mu.Lock()
	if controller.stopped {
		controller.mu.Unlock()
		return observation, errors.New("共享浏览器已停止接单")
	}
	controller.pending.Add(1)
	controller.mu.Unlock()
	transferred := false
	defer func() {
		if !transferred {
			controller.pending.Done()
		}
	}()
	bounded, cancel := context.WithDeadline(ctx, deadline)
	bounded, cancelCall := context.WithTimeout(bounded, controller.options.CallTimeout)
	stopShutdown := context.AfterFunc(controller.lifetime, cancel)
	defer stopShutdown()
	defer cancel()
	defer cancelCall()
	select {
	case controller.gate <- struct{}{}:
	case <-bounded.Done():
		return observation, bounded.Err()
	case <-controller.fatal:
		return observation, errors.New("共享浏览器已失去可靠控制")
	}
	select {
	case <-controller.fatal:
		<-controller.gate
		return observation, errors.New("共享浏览器已失去可靠控制")
	default:
	}
	owned := operation
	operation.files = nil
	transferred = true
	type result struct {
		observation BrowserObservation
		err         error
	}
	completed := make(chan result, 1)
	go func() {
		defer controller.pending.Done()
		defer func() { <-controller.gate }()
		observed, err := controller.perform(bounded, owned)
		completed <- result{observed, err}
	}()
	select {
	case result := <-completed:
		return result.observation, errors.Join(result.err, bounded.Err())
	case <-bounded.Done():
	}
	timer := time.NewTimer(controller.options.CleanupTimeout)
	defer timer.Stop()
	select {
	case result := <-completed:
		return observation, errors.Join(bounded.Err(), result.err)
	case <-timer.C:
		controller.fail()
		return observation, errors.New("共享浏览器启动、关闭或持久记录阻塞")
	}
}

func (controller *BrowserController) save(ctx context.Context, grant BrowserGrant, phase string) error {
	state := BrowserSnapshot{Version: 1, Grant: grant, Phase: phase}
	controller.state = &state
	if err := controller.store.SaveBrowser(ctx, state); err != nil {
		controller.fail()
		return err
	}
	return nil
}

func (controller *BrowserController) perform(ctx context.Context, operation browserOperation) (observation BrowserObservation, failure error) {
	defer func() {
		failure = errors.Join(failure, agentchannel.CloseFiles(operation.files))
		if failure != nil {
			observation = BrowserObservation{}
		}
	}()
	if ctx.Err() != nil {
		return observation, ctx.Err()
	}
	grant, state := operation.grant, controller.state
	if operation.kind == "status" {
		if state == nil || state.Grant != grant {
			return observation, errors.New("共享浏览器状态不属于该授权")
		}
		if state.Phase == "active" {
			select {
			case <-controller.current.scope.Done():
				controller.fail()
				return observation, errors.New("共享浏览器活动实例已经失效")
			default:
			}
		}
		return BrowserObservation{Grant: grant, Phase: state.Phase}, nil
	}
	if operation.kind == "close" {
		if state != nil && state.Grant == grant {
			if state.Phase != "closed" {
				if err := controller.drain(ctx); err != nil {
					return observation, err
				}
			}
			return BrowserObservation{Grant: grant, Phase: "closed"}, nil
		}
		if state != nil && (state.Phase != "closed" || grant.Revision <= state.Grant.Revision || grant.Request.Group.ID == state.Grant.Request.Group.ID) {
			return observation, errors.New("共享浏览器关闭请求过期或实例不符")
		}
		if err := controller.save(ctx, grant, "closed"); err != nil {
			return observation, err
		}
		return BrowserObservation{Grant: grant, Phase: "closed"}, nil
	}
	if state != nil && (state.Phase != "closed" || grant.Revision <= state.Grant.Revision || grant.Request.Group.ID == state.Grant.Request.Group.ID) {
		return observation, errors.New("共享浏览器启动授权已消费或实例复用")
	}
	scope, err := controller.factory.OpenBrowser(grant.Request, operation.files, grant.Limits.options().ShutdownTimeout)
	if err != nil || scope == nil {
		return observation, errors.Join(errors.New("共享浏览器服务组未完成接管"), err)
	}
	operation.files = nil
	instance := &browserInstance{grant: grant, scope: scope, watched: make(chan struct{})}
	controller.current = instance
	stopCancellation := controller.cancelScope(ctx, instance)
	defer func() {
		failure = errors.Join(failure, stopCancellation(), ctx.Err())
		if failure != nil {
			failure = errors.Join(failure, controller.drain(context.WithoutCancel(ctx)))
		}
	}()
	if err := controller.save(ctx, grant, "consumed"); err != nil {
		return observation, err
	}
	options := grant.Limits.options()
	options.UserID, options.ComputerID, options.Generation = grant.Identity.UserID, grant.Identity.ComputerID, grant.Identity.Generation
	options.Logger = controller.logger
	options.Authorize = func(ctx context.Context, session computer.ExecutionSession) error {
		if instance.stopping.Load() || !grant.Identity.matches(session) || session.ContainerID != grant.Request.Group.Container.ContainerID {
			return sharedbrowser.ErrDenied
		}
		return controller.authorize(ctx, session)
	}
	instance.service, err = scope.Start(ctx, grant.Request, options)
	if err != nil || instance.service == nil {
		return observation, errors.Join(errors.New("共享浏览器服务未完成启动"), err)
	}
	select {
	case <-scope.Done():
		return observation, errors.New("共享浏览器启动期间退出")
	default:
	}
	if err := controller.save(ctx, grant, "active"); err != nil {
		return observation, err
	}
	controller.available.Store(instance)
	instance.watching = true
	go controller.watch(instance)
	controller.logger.InfoContext(ctx, "共享浏览器服务已启动", "user_id", grant.Identity.UserID, "computer_id", grant.Identity.ComputerID, "service_id", grant.Request.Group.ID, "revision", grant.Revision)
	return BrowserObservation{Grant: grant, Phase: "active"}, nil
}

// cancelScope 不等待业务门禁；返回函数必须在关闭描述符前汇合撤销回调。
func (controller *BrowserController) cancelScope(ctx context.Context, instance *browserInstance) func() error {
	completed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { instance.stopping.Store(true); completed <- instance.scope.Revoke() })
	return func() error {
		if stop() {
			// Done 可能已关闭而取消通知尚未执行，完成路径不能抢先取消必需的撤销。
			if ctx.Err() != nil {
				instance.stopping.Store(true)
				return instance.scope.Revoke()
			}
			return nil
		}
		return <-completed
	}
}

func (controller *BrowserController) watch(instance *browserInstance) {
	defer close(instance.watched)
	select {
	case <-controller.lifetime.Done():
		return
	case <-instance.scope.Done():
		if !instance.stopping.Load() {
			controller.fail()
		}
	}
}

// drain 只有实际服务组、直接进程及目录均确认结束后才保存关闭屏障；失败不释放控制目录锁。
func (controller *BrowserController) drain(ctx context.Context) error {
	instance := controller.current
	if instance == nil {
		return errors.New("共享浏览器未关闭记录缺少受管实例")
	}
	instance.stopping.Store(true)
	controller.available.CompareAndSwap(instance, nil)
	bounded, cancel := context.WithTimeout(ctx, controller.options.CleanupTimeout)
	defer cancel()
	stopCancellation := controller.cancelScope(bounded, instance)
	failure := instance.scope.Drain(bounded)
	failure = errors.Join(failure, stopCancellation())
	if failure == nil {
		failure = instance.scope.Close()
	}
	if failure == nil && instance.watching {
		select {
		case <-instance.watched:
		case <-bounded.Done():
			failure = bounded.Err()
		}
	}
	if failure == nil {
		failure = controller.save(bounded, instance.grant, "closed")
	}
	if failure != nil {
		controller.fail()
		return failure
	}
	controller.current = nil
	controller.logger.InfoContext(ctx, "共享浏览器服务已关闭并持久确认", "user_id", instance.grant.Identity.UserID, "computer_id", instance.grant.Identity.ComputerID, "service_id", instance.grant.Request.Group.ID, "revision", instance.grant.Revision)
	return nil
}

// Shutdown 先停止接单与在途启动，串行关闭共享服务；不以超时或进程退出代替持久关闭成功。
func (controller *BrowserController) Shutdown(ctx context.Context) error {
	controller.shutdownOnce.Do(func() {
		controller.mu.Lock()
		controller.stopped = true
		controller.available.Store(nil)
		controller.stop()
		controller.mu.Unlock()
		go func() {
			defer close(controller.shutdownDone)
			controller.pending.Wait()
			select {
			case <-controller.fatal:
				controller.shutdownError = errors.New("共享浏览器没有可靠关闭")
				return
			default:
			}
			if controller.current != nil {
				controller.shutdownError = controller.drain(context.Background())
			}
		}()
	})
	select {
	case <-ctx.Done():
		controller.fail()
		return ctx.Err()
	case <-controller.fatal:
		return errors.New("共享浏览器没有可靠关闭")
	case <-controller.shutdownDone:
		return controller.shutdownError
	}
}
