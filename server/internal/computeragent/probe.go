package computeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

// ProbeGrant 由宿主节点生成；Revision 在该电脑的全部代次中递增，未知请求不得重发。
type ProbeGrant struct {
	Identity Identity                 `json:"identity"`
	Revision int64                    `json:"revision"`
	Request  taskprocess.ProbeRequest `json:"request"`
}

func (grant ProbeGrant) Validate() error {
	if grant.Identity.validate() != nil || grant.Revision < 1 || grant.Request.Validate() != nil {
		return errors.New("电脑代理探针授权无效")
	}
	return nil
}

// ProbeSnapshot 只保留最后一次消费屏障；节点负责持久单调版本和不可复用的服务实例。
type ProbeSnapshot struct {
	Version int        `json:"version"`
	Grant   ProbeGrant `json:"grant"`
	Phase   string     `json:"phase"`
}

func (snapshot ProbeSnapshot) validate() error {
	if snapshot.Version != 1 || snapshot.Grant.Validate() != nil || snapshot.Phase != "consumed" && snapshot.Phase != "closed" {
		return errors.New("电脑代理探针记录无效")
	}
	return nil
}

type ProbeStore interface {
	LoadProbe(context.Context) (*ProbeSnapshot, error)
	SaveProbe(context.Context, ProbeSnapshot) error
}

// ProbeScope 在成功 Open 后接管三个入口；Run 只执行固定程序，Drain 必须确认整组为空。
type ProbeScope interface {
	Run(context.Context, taskprocess.ProbeRequest, *os.File) (computerprobe.Evidence, error)
	Revoke() error
	Drain(context.Context) error
	Close() error
}

type ProbeFactory interface {
	OpenProbe(processgroup.ServiceReference, []*os.File) (ProbeScope, error)
}

// ProbeController 串行执行容器级检查，不占用 Bot 桌面锁；失控时通知容器入口立即退出。
type ProbeController struct {
	identity  Identity
	store     ProbeStore
	factory   ProbeFactory
	options   Options
	logger    *slog.Logger
	onFatal   func()
	gate      chan struct{}
	mu        sync.Mutex
	stopped   bool
	pending   sync.WaitGroup
	fatal     chan struct{}
	fatalOnce sync.Once
	state     *ProbeSnapshot
	lifetime  context.Context
	stop      context.CancelFunc
}

func NewProbeController(ctx context.Context, identity Identity, store ProbeStore, factory ProbeFactory, options Options, logger *slog.Logger, onFatal func()) (*ProbeController, error) {
	if identity.validate() != nil || store == nil || factory == nil || options.validate() != nil || logger == nil || onFatal == nil {
		return nil, errors.New("电脑代理探针监督配置无效")
	}
	ctx, cancel := context.WithTimeout(ctx, options.CallTimeout)
	defer cancel()
	state, err := store.LoadProbe(ctx)
	if err != nil {
		return nil, err
	}
	if state != nil {
		previous := state.Grant.Identity
		if state.validate() != nil || previous.UserID != identity.UserID || previous.ComputerID != identity.ComputerID || previous.NodeID != identity.NodeID || previous.Generation > identity.Generation {
			return nil, errors.New("电脑代理探针持久身份不符")
		}
		if state.Phase != "closed" {
			if previous.Generation == identity.Generation {
				return nil, errors.New("电脑代理旧探针未确认关闭，需要隔离当前容器")
			}
			closed := *state
			closed.Phase = "closed"
			if err := store.SaveProbe(ctx, closed); err != nil {
				return nil, err
			}
			state = &closed
		}
	}
	lifetime, stop := context.WithCancel(context.Background())
	return &ProbeController{identity: identity, store: store, factory: factory, options: options, logger: logger, onFatal: onFatal, gate: make(chan struct{}, 1), fatal: make(chan struct{}), state: state, lifetime: lifetime, stop: stop}, nil
}

func (controller *ProbeController) fail() {
	controller.fatalOnce.Do(func() {
		close(controller.fatal)
		controller.logger.Error("电脑代理失去探针控制，必须退出容器", "user_id", controller.identity.UserID, "computer_id", controller.identity.ComputerID, "generation", controller.identity.Generation)
		controller.onFatal()
	})
}

// Check 接管传入描述符；返回证据前必须撤销授权、回收进程并持久关闭，未知结果不自动重试。
func (controller *ProbeController) Check(ctx context.Context, grant ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
	deadline, err := grant.Request.Deadline.Local(taskprocess.ProbeMaximumLifetime)
	if err != nil {
		return computerprobe.Evidence{}, errors.Join(err, agentchannel.CloseFiles(files))
	}
	return controller.checkAt(ctx, grant, files, deadline)
}

func (controller *ProbeController) checkAt(ctx context.Context, grant ProbeGrant, files []*os.File, deadline time.Time) (evidence computerprobe.Evidence, failure error) {
	defer func() {
		failure = errors.Join(failure, agentchannel.CloseFiles(files))
		if failure != nil {
			evidence = computerprobe.Evidence{}
		}
	}()
	if grant.Validate() != nil || grant.Identity != controller.identity || len(files) != 3 || !time.Now().Before(deadline) {
		return evidence, errors.New("电脑代理探针请求身份、入口或期限无效")
	}
	controller.mu.Lock()
	if controller.stopped {
		controller.mu.Unlock()
		return evidence, errors.New("电脑代理探针已停止接单")
	}
	controller.pending.Add(1)
	controller.mu.Unlock()
	// 已登记的请求始终计入 Shutdown；外层超时也不能提前释放仍在使用的控制入口。
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
		return evidence, bounded.Err()
	case <-controller.fatal:
		return evidence, errors.New("电脑代理探针已失去可靠控制")
	}
	select {
	case <-controller.fatal:
		<-controller.gate
		return evidence, errors.New("电脑代理探针已失去可靠控制")
	default:
	}
	owned := files
	files = nil
	transferred = true
	type result struct {
		evidence computerprobe.Evidence
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		defer controller.pending.Done()
		defer func() { <-controller.gate }()
		proof, err := controller.perform(bounded, grant, owned)
		completed <- result{proof, err}
	}()
	select {
	case result := <-completed:
		return result.evidence, errors.Join(result.err, bounded.Err())
	case <-bounded.Done():
	}
	// 本地文件系统或进程收尾可能不响应取消；独立等待上限保证入口仍能终止整个容器。
	timer := time.NewTimer(controller.options.CleanupTimeout)
	defer timer.Stop()
	select {
	case result := <-completed:
		return computerprobe.Evidence{}, errors.Join(bounded.Err(), result.err)
	case <-timer.C:
		controller.fail()
		return evidence, errors.New("电脑代理探针启动、回收或记录提交阻塞")
	}
}

func (controller *ProbeController) perform(ctx context.Context, grant ProbeGrant, files []*os.File) (evidence computerprobe.Evidence, failure error) {
	defer func() {
		failure = errors.Join(failure, agentchannel.CloseFiles(files))
		if failure != nil {
			evidence = computerprobe.Evidence{}
		}
	}()
	if ctx.Err() != nil {
		return evidence, ctx.Err()
	}
	if controller.state != nil && (controller.state.Phase != "closed" || grant.Revision <= controller.state.Grant.Revision || grant.Request.Group.ID == controller.state.Grant.Request.Group.ID) {
		return evidence, errors.New("电脑代理探针已消费、版本过期或实例复用")
	}
	scope, err := controller.factory.OpenProbe(grant.Request.Group, files)
	if err != nil {
		return evidence, err
	}
	files = nil
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		controller.fail()
		return evidence, errors.Join(errors.New("电脑代理探针缺少执行期限"), scope.Drain(ctx), scope.Close())
	}
	signaled := make(chan error, 1)
	permit, err := newPermit(deadline, grant.Revision, make(chan struct{}, 1), func() { go func() { signaled <- scope.Revoke() }() })
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), controller.options.CleanupTimeout)
		defer cancel()
		failure = errors.Join(err, scope.Drain(cleanup), scope.Close())
		controller.fail()
		return evidence, failure
	}
	canceled := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(canceled)
		if permit.revoke() != nil {
			controller.fail()
		}
	})
	defer func() {
		if !stopCancellation() {
			<-canceled
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), controller.options.CleanupTimeout)
		defer cancel()
		revokeErr := permit.revoke()
		var signalErr error
		select {
		case signalErr = <-signaled:
		case <-cleanup.Done():
			signalErr = errors.New("电脑代理探针终止信号未确认")
		}
		cleanupErr := errors.Join(revokeErr, signalErr, scope.Drain(cleanup))
		if cleanupErr == nil {
			cleanupErr = scope.Close()
		}
		cleanupErr = errors.Join(cleanupErr, permit.close())
		if cleanupErr == nil && controller.state != nil && controller.state.Grant == grant {
			closed := *controller.state
			closed.Phase = "closed"
			cleanupErr = controller.store.SaveProbe(cleanup, closed)
			if cleanupErr == nil {
				controller.state = &closed
			}
		}
		if cleanupErr != nil {
			controller.fail()
		}
		failure = errors.Join(failure, cleanupErr)
		if failure != nil {
			evidence = computerprobe.Evidence{}
		}
	}()
	state := ProbeSnapshot{Version: 1, Grant: grant, Phase: "consumed"}
	controller.state = &state
	if err = controller.store.SaveProbe(ctx, state); err != nil {
		controller.fail()
		return evidence, err
	}
	if ctx.Err() != nil || !permit.active(grant.Revision) {
		return evidence, errors.New("电脑代理探针授权已结束")
	}
	evidence, err = scope.Run(ctx, grant.Request, permit.reader)
	if err != nil {
		return computerprobe.Evidence{}, err
	}
	if ctx.Err() != nil || !permit.active(grant.Revision) || evidence.Validate(grant.Request.Group.ID) != nil {
		return computerprobe.Evidence{}, errors.New("电脑代理探针证据或完成期限无效")
	}
	controller.logger.InfoContext(ctx, "电脑代理基础探针检查完成，等待回收确认", "user_id", grant.Identity.UserID, "computer_id", grant.Identity.ComputerID, "service_id", grant.Request.Group.ID, "revision", grant.Revision)
	return evidence, nil
}

// Shutdown 在停止协议接入后等待所有探针收尾；未知控制状态禁止释放持久目录锁。
func (controller *ProbeController) Shutdown(ctx context.Context) error {
	controller.mu.Lock()
	controller.stopped = true
	controller.stop()
	controller.mu.Unlock()
	done := make(chan struct{})
	go func() { controller.pending.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-controller.fatal:
		return errors.New("电脑代理探针未可靠关闭")
	case <-done:
	}
	select {
	case <-controller.fatal:
		return errors.New("电脑代理探针未可靠关闭")
	default:
		return nil
	}
}
