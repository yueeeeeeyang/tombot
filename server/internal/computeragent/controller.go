package computeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

// StoreCatalog 在代理接单前遍历持久桌面，业务文件不属于此控制目录。
type StoreCatalog interface {
	Desktop(string) (Store, error)
	Visit(context.Context, func(Snapshot) error) error
}

type desktopCapacity struct {
	mu      sync.Mutex
	maximum int
	active  map[string]struct{}
}

func (capacity *desktopCapacity) reserve(desktopID string) error {
	capacity.mu.Lock()
	defer capacity.mu.Unlock()
	if _, exists := capacity.active[desktopID]; exists {
		return nil
	}
	if len(capacity.active) >= capacity.maximum {
		return errors.New("电脑代理活动桌面已达到并发上限")
	}
	capacity.active[desktopID] = struct{}{}
	return nil
}

func (capacity *desktopCapacity) release(desktopID string) {
	capacity.mu.Lock()
	defer capacity.mu.Unlock()
	delete(capacity.active, desktopID)
}

type desktopEntry struct {
	identity computer.DesktopIdentity
	load     *desktopLoad
	watching bool
}

type desktopLoad struct {
	ready chan struct{}
	guard *desktopGuard
	err   error
}

// Controller 维护同一用户容器内的独立桌面授权，全局锁只保护目录和接单状态，不等待远端调用或刷盘。
type Controller struct {
	identity         Identity
	store            StoreCatalog
	factory          ScopeFactory
	options          Options
	logger           *slog.Logger
	capacity         *desktopCapacity
	mu               sync.Mutex
	desktops         map[string]*desktopEntry
	bots             map[string]string
	displays         map[int]string
	stopped, serving bool
	pending          sync.WaitGroup
	wake             chan struct{}
	fatal            chan struct{}
	fatalOnce        sync.Once
}

// NewController 在提供控制协议前核验全部持久桌面；任一未知旧授权或冲突身份都阻止启动。
func NewController(ctx context.Context, identity Identity, store StoreCatalog, factory ScopeFactory, options Options, maximum int, logger *slog.Logger) (*Controller, error) {
	if identity.validate() != nil || store == nil || factory == nil || options.validate() != nil || maximum < 1 || maximum > computer.MaxConcurrentDesktops || logger == nil {
		return nil, errors.New("电脑代理多桌面配置无效")
	}
	controller := &Controller{identity: identity, store: store, factory: factory, options: options, logger: logger,
		capacity: &desktopCapacity{maximum: maximum, active: map[string]struct{}{}}, desktops: map[string]*desktopEntry{}, bots: map[string]string{}, displays: map[int]string{}, wake: make(chan struct{}, 1), fatal: make(chan struct{})}
	ctx, cancel := context.WithTimeout(ctx, options.CallTimeout)
	defer cancel()
	err := store.Visit(ctx, func(snapshot Snapshot) error {
		if snapshot.validate() != nil {
			return errors.New("电脑代理持久桌面记录无效")
		}
		_, err := controller.desktop(ctx, snapshot.Command.Session.DesktopIdentity, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return controller, nil
}

func (controller *Controller) fail() { controller.fatalOnce.Do(func() { close(controller.fatal) }) }

func (controller *Controller) begin(ctx context.Context) (context.Context, func(), error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if controller.stopped {
		return nil, nil, errors.New("电脑代理已停止接收桌面操作")
	}
	select {
	case <-controller.fatal:
		return nil, nil, errors.New("电脑代理失去可靠控制，必须退出容器")
	default:
	}
	controller.pending.Add(1)
	bounded, cancel := context.WithTimeout(ctx, controller.options.CallTimeout)
	return bounded, func() { cancel(); controller.pending.Done() }, nil
}

func (controller *Controller) desktop(ctx context.Context, identity computer.DesktopIdentity, create bool) (*desktopGuard, error) {
	if identity.Validate() != nil || identity.UserID != controller.identity.UserID || identity.ComputerID != controller.identity.ComputerID {
		return nil, errors.New("电脑代理桌面归属无效")
	}
	controller.mu.Lock()
	entry, exists := controller.desktops[identity.DesktopID]
	if exists && entry.identity != identity {
		controller.mu.Unlock()
		return nil, errors.New("电脑代理拒绝替换稳定桌面身份")
	}
	if !exists {
		if !create || len(controller.desktops) >= computer.MaxDesktops || controller.bots[identity.BotID] != "" || controller.displays[identity.DisplayNumber] != "" {
			controller.mu.Unlock()
			return nil, errors.New("电脑代理桌面不存在、数量超限或运行身份冲突")
		}
		entry = &desktopEntry{identity: identity, load: &desktopLoad{ready: make(chan struct{})}}
		controller.desktops[identity.DesktopID] = entry
		controller.bots[identity.BotID] = identity.DesktopID
		controller.displays[identity.DisplayNumber] = identity.DesktopID
	}
	// 初始化不发放资源控制权；失败的读取可以在下一次显式请求中重新核验，不能跳过损坏记录。
	if exists {
		select {
		case <-entry.load.ready:
			if entry.load.err != nil {
				entry.load = &desktopLoad{ready: make(chan struct{})}
				exists = false
			}
		default:
		}
	}
	load := entry.load
	controller.mu.Unlock()
	if !exists {
		// 初始化在桌面自己的 ready 屏障内进行，其他桌面的加载、续约和撤销不等待此处 I/O。
		record, err := controller.store.Desktop(identity.DesktopID)
		if err == nil {
			load.guard, err = newGuard(ctx, controller.identity, identity, controller.capacity, record, controller.factory, controller.options, controller.logger)
		}
		load.err = err
		if err != nil {
			controller.logger.WarnContext(ctx, "电脑代理桌面初始化失败，未发放执行授权", "user_id", identity.UserID, "computer_id", identity.ComputerID, "desktop_id", identity.DesktopID)
		}
		close(load.ready)
		select {
		case controller.wake <- struct{}{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-load.ready:
		return load.guard, load.err
	}
}

// Open 接管全部描述符，按完整桌面身份授权；不同桌面的版本与资源组互不覆盖。
func (controller *Controller) Open(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, files []*os.File) (computer.SessionObservation, error) {
	return controller.openAt(ctx, command, reference, files, time.Now().Add(command.Lifetime))
}

func (controller *Controller) openAt(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, files []*os.File, deadline time.Time) (observation computer.SessionObservation, failure error) {
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	ctx, leave, err := controller.begin(ctx)
	if err != nil {
		return observation, err
	}
	defer leave()
	if command.Validate() != nil || !controller.identity.matches(command.Session) || reference.Validate() != nil || reference.Session != command.Session || len(files) != 4 || !time.Now().Before(deadline) {
		return observation, errors.New("电脑代理桌面开启授权无效")
	}
	guard, err := controller.desktop(ctx, command.Session.DesktopIdentity, true)
	if err != nil {
		return observation, err
	}
	observation, err = guard.openAt(ctx, command, reference, files, deadline)
	files = nil
	return observation, err
}

// Renew 只更新所属桌面的有效授权；重复版本不延长期限，未知桌面不会因续约而创建。
func (controller *Controller) Renew(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	return controller.renewAt(ctx, command, time.Now().Add(command.Lifetime))
}

func (controller *Controller) renewAt(ctx context.Context, command computer.SessionCommand, deadline time.Time) (computer.SessionObservation, error) {
	ctx, leave, err := controller.begin(ctx)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	defer leave()
	if command.Validate() != nil || !controller.identity.matches(command.Session) {
		return computer.SessionObservation{}, errors.New("电脑代理桌面续约授权无效")
	}
	guard, err := controller.desktop(ctx, command.Session.DesktopIdentity, false)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	return guard.renewAt(ctx, command, deadline)
}

// Close 定向撤销并持久关闭一个桌面；先行关闭也保存屏障，不撤销其他桌面的授权。
func (controller *Controller) Close(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	ctx, leave, err := controller.begin(ctx)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	defer leave()
	if command.Validate() != nil || !controller.identity.matches(command.Session) {
		return computer.SessionObservation{}, errors.New("电脑代理桌面关闭授权无效")
	}
	guard, err := controller.desktop(ctx, command.Session.DesktopIdentity, true)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	return guard.Close(ctx, command)
}

// WithTask 只借出目标桌面的入组和撤销入口；回调受该桌面锁保护，不阻塞其他桌面。
func (controller *Controller) WithTask(ctx context.Context, session computer.ExecutionSession, role string, start func(context.Context, processgroup.Reference, *os.File, *os.File) error) error {
	ctx, leave, err := controller.begin(ctx)
	if err != nil {
		return err
	}
	defer leave()
	if session.Validate() != nil || !controller.identity.matches(session) || start == nil {
		return errors.New("电脑代理桌面任务授权无效")
	}
	guard, err := controller.desktop(ctx, session.DesktopIdentity, false)
	if err != nil {
		return err
	}
	return guard.WithTask(ctx, session, role, start)
}

// Serve 为每个已登记桌面启动独立看门狗，致命失控会立即通知入口退出整个容器。
func (controller *Controller) Serve(ctx context.Context) error {
	controller.mu.Lock()
	if controller.serving || controller.stopped {
		controller.mu.Unlock()
		return errors.New("电脑代理桌面看门狗已启动或已停止")
	}
	controller.serving = true
	controller.mu.Unlock()
	running, cancel := context.WithCancel(ctx)
	var watchers sync.WaitGroup
	defer func() { cancel(); watchers.Wait() }()
	for {
		controller.mu.Lock()
		for _, entry := range controller.desktops {
			load := entry.load
			select {
			case <-load.ready:
				if load.err != nil || entry.watching {
					continue
				}
				entry.watching = true
				watchers.Add(1)
				go func() {
					defer watchers.Done()
					err := load.guard.Serve(running)
					if running.Err() == nil || !errors.Is(err, running.Err()) {
						controller.fail()
					}
				}()
			default:
			}
		}
		controller.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-controller.fatal:
			return errors.New("电脑代理桌面失去可靠控制，必须退出容器")
		case <-controller.wake:
		}
	}
}

// Shutdown 封闭接单、等待在途操作，再有界并行回收全部桌面；失败时不能关闭持久目录锁。
func (controller *Controller) Shutdown(ctx context.Context) error {
	controller.mu.Lock()
	controller.stopped = true
	controller.mu.Unlock()
	done := make(chan struct{})
	go func() { controller.pending.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	controller.mu.Lock()
	jobs := make(chan *desktopGuard, len(controller.desktops))
	var failures error
	for _, entry := range controller.desktops {
		if entry.load.err != nil {
			failures = errors.Join(failures, entry.load.err)
		} else {
			jobs <- entry.load.guard
		}
	}
	close(jobs)
	workers := min(controller.capacity.maximum, len(jobs))
	controller.mu.Unlock()
	results := make(chan error, workers)
	for range workers {
		go func() {
			var failure error
			for guard := range jobs {
				failure = errors.Join(failure, guard.Shutdown(ctx))
			}
			results <- failure
		}()
	}
	for range workers {
		select {
		case <-ctx.Done():
			return errors.Join(failures, ctx.Err())
		case err := <-results:
			failures = errors.Join(failures, err)
		}
	}
	return failures
}
