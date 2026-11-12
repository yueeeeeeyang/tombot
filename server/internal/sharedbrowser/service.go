package sharedbrowser

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
)

type entry struct {
	window         Window
	session        computer.ExecutionSession
	target         target.ID
	ctx            context.Context
	stop           context.CancelFunc
	life           context.Context
	revoke         context.CancelFunc
	gate           chan struct{}
	last           Command
	lastError      error
	active         *commandRun
	dialogGate     chan struct{}
	dialogSequence int64
	dialogReply    DialogReply
	dialogError    error
	uploadBatches  []string
}

type Service struct {
	options     Options
	root        context.Context
	stop        context.CancelFunc
	connection  chromedp.Transport
	browser     *chromedp.Browser
	listTargets func(context.Context) ([]*target.Info, error)
	mu          sync.Mutex
	windows     map[string]*entry
	targets     map[target.ID]*entry
	creation    chan struct{}
	events      chan any
	done        chan struct{}
	failed      bool
	closed      bool
	commands    sync.WaitGroup
	uploads     sync.WaitGroup
	uploadGate  chan struct{}
}

// New 接管 Chrome 的两端私有管道并核验浏览器能力；启动失败关闭管道，不留下可继续使用的半初始化实例。
// 监督器负责非 root 浏览器、独立服务资源组、私有 profile、出网代理和最终进程回收。
func New(ctx context.Context, reader io.ReadCloser, writer io.WriteCloser, options Options) (*Service, error) {
	if ctx == nil || reader == nil || writer == nil || options.Validate() != nil {
		return nil, ErrInvalid
	}
	connection := chromedp.NewPipeConn(&frameReader{ReadCloser: reader}, frameWriter{writer})
	life, stop := context.WithCancel(ctx)
	allocator := &pipeAllocator{connection: connection}
	base, _ := chromedp.NewAllocatorContext(life, allocator)
	root, _ := chromedp.NewContext(base, chromedp.WithErrorf(func(string, ...any) {
		options.Logger.Warn("共享浏览器协议异常", "user_id", options.UserID, "computer_id", options.ComputerID)
	}))
	service := &Service{options: options, root: root, stop: stop, connection: connection, windows: map[string]*entry{}, targets: map[target.ID]*entry{}, creation: make(chan struct{}, 1), events: make(chan any, options.MaximumEvents), done: make(chan struct{}), uploadGate: make(chan struct{}, 1)}
	// 首次 chromedp 初始化必须在长期上下文执行；只由超时取消，不能让请求结束关闭整个浏览器。
	timer := time.AfterFunc(options.CallTimeout, stop)
	err := chromedp.Do(root)
	timer.Stop()
	if err == nil {
		service.browser = chromedp.FromContext(root).Browser
		service.listTargets = func(ctx context.Context) ([]*target.Info, error) {
			listed, err := cdp.Call(ctx, service.browser, target.GetTargets, target.GetTargetsParams{})
			return listed.TargetInfos, err
		}
		startup, cancel := context.WithTimeout(root, options.CallTimeout)
		defer cancel()
		_, err = cdp.Call(startup, service.browser, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{Behavior: browser.SetDownloadBehaviorBehaviorDeny})
		if err == nil {
			// 不恢复上次运行的无主页面；只有新授权窗口可以继续执行网页脚本。
			var result page.NavigateResult
			result, err = cdp.Call(startup, chromedp.FromContext(root).Target, page.Navigate, page.NavigateParams{URL: "about:blank"})
			if err == nil && result.ErrorText != "" {
				err = ErrUnavailable
			}
		}
		if err == nil {
			err = service.closeUnowned(startup)
		}
		if err == nil {
			err = chromedp.ListenBrowser(root, service.observe)
		}
		if err == nil {
			_, err = cdp.Call(startup, service.browser, target.SetDiscoverTargets, target.SetDiscoverTargetsParams{Discover: true})
		}
	}
	if err != nil {
		stop()
		var closeError error
		if options.Uploads != nil {
			closeError = options.Uploads.Close()
		}
		return nil, errors.Join(ErrUnavailable, connection.Close(), closeError)
	}
	go service.runEvents()
	return service, nil
}

func (service *Service) authorize(ctx context.Context, session computer.ExecutionSession) error {
	if session.Validate() != nil || session.UserID != service.options.UserID || session.ComputerID != service.options.ComputerID || session.Generation != service.options.Generation {
		return ErrDenied
	}
	service.mu.Lock()
	unavailable := service.closed || service.failed
	service.mu.Unlock()
	if unavailable || service.root.Err() != nil {
		return ErrUnavailable
	}
	if service.options.Authorize(ctx, session) != nil {
		return ErrDenied
	}
	return ctx.Err()
}
func acquire(ctx context.Context, gate chan struct{}) error {
	select {
	case gate <- struct{}{}:
		if ctx.Err() != nil {
			<-gate
			return ctx.Err()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (service *Service) bounded(parent, caller context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, service.options.CallTimeout)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }
}

func (service *Service) fail() {
	service.mu.Lock()
	changed := !service.failed && !service.closed
	if changed {
		service.failed = true
	}
	service.mu.Unlock()
	service.stop()
	if changed {
		service.options.Logger.Error("共享浏览器状态未确认，撤销全部窗口", "user_id", service.options.UserID, "computer_id", service.options.ComputerID)
	}
}

// Open 使用持久操作 ID 幂等创建空白独立窗口；后续导航使用窗口自己的有序命令。
func (service *Service) Open(ctx context.Context, address Address) (Window, error) {
	if _, err := ulid.ParseStrict(address.WindowID); err != nil {
		return Window{}, ErrInvalid
	}
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	if err := service.authorize(bounded, address.Session); err != nil {
		return Window{}, err
	}
	if err := acquire(bounded, service.creation); err != nil {
		return Window{}, err
	}
	defer func() { <-service.creation }()
	if err := service.authorize(bounded, address.Session); err != nil {
		return Window{}, err
	}
	service.mu.Lock()
	if old := service.windows[address.WindowID]; old != nil {
		window := old.window
		own := old.session == address.Session
		service.mu.Unlock()
		if !own {
			return Window{}, ErrDenied
		}
		if window.State == "closed" {
			return Window{}, ErrConflict
		}
		if window.State != "ready" {
			return Window{}, ErrUnknown
		}
		return window, nil
	}
	full := service.full(address.Session)
	service.mu.Unlock()
	if full {
		return Window{}, ErrCapacity
	}
	positive := true
	created, err := cdp.Call(bounded, service.browser, target.CreateTarget, target.CreateTargetParams{URL: "about:blank", NewWindow: &positive, Background: &positive, Width: int64(service.options.Width), Height: int64(service.options.Height + 120)})
	if err != nil || created.TargetID == "" {
		service.fail()
		return Window{}, ErrUnknown
	}
	window, err := service.attach(bounded, address, created.TargetID, "")
	if err != nil {
		service.fail()
		return Window{}, ErrUnknown
	}
	if err := service.authorize(bounded, address.Session); err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), service.options.ShutdownTimeout)
		defer stop()
		service.mu.Lock()
		item := service.windows[address.WindowID]
		service.mu.Unlock()
		if closeErr := service.closeWindow(cleanup, item); closeErr != nil {
			service.fail()
		}
		return Window{}, ErrDenied
	}
	return window, nil
}

// full 在元数据锁内计数；关闭回执仍保留到会话撤销，防止迟到的创建请求重新打开页面。
func (service *Service) full(session computer.ExecutionSession) bool {
	count := 0
	for _, item := range service.targets {
		if item.session == session {
			count++
		}
	}
	return len(service.targets) >= service.options.MaximumWindows || count >= service.options.MaximumPerSession || len(service.windows) >= service.options.MaximumWindowRecords
}

func (service *Service) attach(ctx context.Context, address Address, id target.ID, parent string) (Window, error) {
	pageContext, stop := chromedp.NewContext(service.root, chromedp.WithTargetID(id), chromedp.WithDetachOnCancel())
	interrupt := context.AfterFunc(ctx, func() { service.fail() })
	err := chromedp.Do(pageContext)
	interrupt()
	if err != nil || ctx.Err() != nil {
		stop()
		return Window{}, ErrUnavailable
	}
	tab := chromedp.FromContext(pageContext).Target
	_, err = cdp.Call(ctx, tab, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{Width: int64(service.options.Width), Height: int64(service.options.Height), DeviceScaleFactor: 1})
	if err == nil {
		_, err = cdp.Call(ctx, tab, emulation.SetFocusEmulationEnabled, emulation.SetFocusEmulationEnabledParams{Enabled: true})
	}
	if err != nil {
		stop()
		return Window{}, ErrUnavailable
	}
	life, revoke := context.WithCancel(service.root)
	item := &entry{window: Window{ID: address.WindowID, BotID: address.Session.BotID, DesktopID: address.Session.DesktopID, ParentID: parent, State: "ready", URL: "about:blank"}, session: address.Session, target: id, ctx: pageContext, stop: stop, life: life, revoke: revoke, gate: make(chan struct{}, 1), dialogGate: make(chan struct{}, 1)}
	if err := chromedp.Listen(pageContext, func(event any) { service.observeDialog(item, event) }); err != nil {
		revoke()
		stop()
		return Window{}, ErrUnavailable
	}
	window := item.window
	service.mu.Lock()
	service.windows[address.WindowID], service.targets[id] = item, item
	service.mu.Unlock()
	return window, nil
}

// List 只返回完整会话所属窗口；不同 Bot 以及同 Bot 旧运行均不能枚举。
func (service *Service) List(ctx context.Context, session computer.ExecutionSession) ([]Window, error) {
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	if err := service.authorize(bounded, session); err != nil {
		return nil, err
	}
	// Chrome 的目标事件可能仍保留导航前标题；每次列表只查询一次完整快照，避免逐窗口调用。
	if service.listTargets == nil {
		return nil, ErrUnavailable
	}
	listed, err := service.listTargets(bounded)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, bounded.Err())
	}
	metadata := make(map[target.ID]*target.Info, len(listed))
	for _, info := range listed {
		if info == nil || info.TargetID == "" || metadata[info.TargetID] != nil {
			return nil, ErrUnavailable
		}
		metadata[info.TargetID] = info
	}
	if err := service.authorize(bounded, session); err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	result := []Window{}
	for _, item := range service.windows {
		if item.session == session && item.window.State != "closed" {
			info := metadata[item.target]
			if info == nil || info.Type != "page" || len(info.URL) > 8192 || len(info.Title) > 4096 {
				return nil, ErrUnavailable
			}
			window := item.window
			window.URL, window.Title = info.URL, info.Title
			result = append(result, window)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (service *Service) own(ctx context.Context, address Address) (*entry, error) {
	if err := service.authorize(ctx, address.Session); err != nil {
		return nil, err
	}
	service.mu.Lock()
	item := service.windows[address.WindowID]
	service.mu.Unlock()
	if item == nil || item.session != address.Session {
		return nil, ErrDenied
	}
	return item, nil
}

// Close 关闭整个用户浏览器的私有通道并等待事件循环结束；监督器还必须确认 Chrome 整组退出。
func (service *Service) Close(ctx context.Context) error {
	service.mu.Lock()
	service.closed = true
	service.mu.Unlock()
	service.stop()
	err := service.connection.Close()
	bounded, cancel := context.WithTimeout(ctx, service.options.ShutdownTimeout)
	defer cancel()
	completed := make(chan struct{})
	go func() { service.uploads.Wait(); service.commands.Wait(); close(completed) }()
	select {
	case <-completed:
	case <-bounded.Done():
		return errors.Join(ErrUnavailable, err)
	}
	select {
	case <-service.done:
		if service.options.Uploads != nil {
			err = errors.Join(err, service.options.Uploads.Close())
		}
		return err
	case <-bounded.Done():
		return errors.Join(ErrUnavailable, err)
	}
}

// Done 在共享浏览器退出或失联后关闭；调用方必须将其当作整台用户电脑的共享能力故障。
func (service *Service) Done() <-chan struct{} { return service.done }
