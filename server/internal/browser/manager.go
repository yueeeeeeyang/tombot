// Package browser 管理单一持久 Chromium 与 Bot 标签归属，不提供任意脚本执行入口。
package browser

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

const MaxTabs = 32
const MaxOwnerTabs = 8
const OperationTimeout = 30 * time.Second
const ViewportWidth = 1280
const ViewportHeight = 720
const maxRecoveries = 3

type Options struct {
	Executable          string
	ProfileDir          string
	Logger              *slog.Logger
	PersistPopup        func(context.Context, string, Tab) error
	AuthorizeNavigation func(context.Context, Navigation) error
}

type Tab struct {
	ID                string             `json:"id"`
	BotID             string             `json:"bot_id"`
	URL               string             `json:"url"`
	Title             string             `json:"title"`
	Revision          uint64             `json:"revision"`
	ErrorCode         string             `json:"error_code,omitempty"`
	BlockedNavigation *BlockedNavigation `json:"blocked_navigation,omitempty"`
}

// Selection 将归属与预期页面版本一起传入命令；版本为零只适用于不依赖旧画面的读取。
type Selection struct {
	Owner    string
	ID       string
	Revision uint64
}

// Restore 只装载持久标签，不启动进程；必须在首次启动前调用，已有 ID 与页面版本保持不变。
func (m *Manager) Restore(ctx context.Context, tabs []Tab) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	if m.started || len(m.tabs) != 0 || m.closed {
		return invalid("Restore tabs before starting the browser.")
	}
	loaded := map[string]*entry{}
	owners := map[string]int{}
	for _, tab := range tabs {
		if _, err := ulid.ParseStrict(tab.ID); err != nil {
			return invalid("The restored tab ID is invalid.")
		}
		if tab.BotID == "" || len(tab.BotID) > 128 || tab.Revision == 0 || loaded[tab.ID] != nil {
			return invalid("Invalid restored tab metadata.")
		}
		if tab.URL != "about:blank" {
			if _, err := NormalizeURL(tab.URL); err != nil {
				return err
			}
		}
		owners[tab.BotID]++
		if len(loaded) >= MaxTabs || owners[tab.BotID] > MaxOwnerTabs {
			return domain.NewError(409, "BROWSER_TAB_LIMIT", "The saved tabs exceed browser capacity.")
		}
		loaded[tab.ID] = &entry{Tab: tab}
	}
	m.tabs = loaded
	return nil
}

type Status struct {
	State            string `json:"state"`
	Tabs             int    `json:"tabs"`
	RecoveryFailures int    `json:"recovery_failures"`
}

// Status 返回进程生命周期状态，不为查询启动进程；连续失败达到上限后需显式恢复。
func (m *Manager) Status(parent context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return Status{}, err
	}
	defer m.release()
	state := "stopped"
	if !m.closed && m.root.Err() == nil && m.started {
		state = "recovering"
		if m.alive() {
			state = "running"
		} else if m.failures >= maxRecoveries {
			state = "failed"
		}
		if m.navigationFailed.Load() || m.downloadFailed.Load() {
			state = "failed"
		}
	}
	return Status{State: state, Tabs: len(m.tabs), RecoveryFailures: m.failures}, nil
}

type entry struct {
	Tab
	ctx          context.Context
	stop         context.CancelFunc
	target       target.ID
	epoch        atomic.Uint64
	refsEpoch    uint64
	appliedEpoch uint64
	refs         map[string]reference
}

// Manager 的命令门禁包含排队时间；浏览器操作串行，防止快照与交互交错后错误复用引用。
// Changed 只通知状态可能改变，订阅者必须读取 List；通知合并而不缓存无界事件队列。
type Manager struct {
	guard            *navigationGuard
	navigationFailed atomic.Bool
	downloadFailed   atomic.Bool
	options          Options
	root             context.Context
	stop             context.CancelFunc
	gate             chan struct{}
	wake             chan struct{}
	changed          chan struct{}
	wg               sync.WaitGroup
	browser          context.Context
	stopBrowser      context.CancelFunc
	stopAllocator    context.CancelFunc
	tabs             map[string]*entry
	closed, started  bool
	failures         int
	nextRecovery     time.Time
	refSequence      uint64
}

// New 校验私有 profile 路径，但不启动进程；首次 Open 才分配浏览器。调用方必须 Close。
func New(parent context.Context, options Options) (*Manager, error) {
	if parent == nil || options.Logger == nil || !filepath.IsAbs(options.Executable) || !filepath.IsAbs(options.ProfileDir) {
		return nil, errors.New("浏览器依赖或绝对路径不完整")
	}
	if err := os.MkdirAll(options.ProfileDir, 0700); err != nil {
		return nil, err
	}
	stat, err := os.Lstat(options.ProfileDir)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.New("浏览器 profile 必须是仅当前用户可访问的真实目录")
	}
	root, stop := context.WithCancel(parent)
	m := &Manager{options: options, root: root, stop: stop, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1), changed: make(chan struct{}, 1), tabs: map[string]*entry{}}
	m.wg.Add(1)
	go m.supervise()
	return m, nil
}

// NormalizeURL 仅接受绝对 HTTP(S) 地址；拒绝 URL 凭据及本地文件、脚本和浏览器内部协议。
func NormalizeURL(address string) (string, error) {
	if len(address) == 0 || len(address) > 8192 || strings.TrimSpace(address) != address || strings.ContainsAny(address, "\r\n\t\\") {
		return "", invalid("Provide an absolute HTTP or HTTPS URL of at most 8192 bytes.")
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", invalid("Only absolute HTTP and HTTPS URLs without credentials are allowed.")
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func invalid(message string) error { return domain.NewError(400, "INVALID_BROWSER_INPUT", message) }
func unavailable() error {
	return domain.NewError(503, "BROWSER_UNAVAILABLE", "The browser is unavailable. Wait for recovery before sending a new action.")
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) notify() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}
func (m *Manager) Changed() <-chan struct{} { return m.changed }

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.gate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		m.release()
		return err
	}
	return nil
}
func (m *Manager) release() { <-m.gate }
func (m *Manager) alive() bool {
	if m.browser == nil {
		return false
	}
	connection := chromedp.FromContext(m.browser).Browser
	if connection == nil {
		return false
	}
	select {
	case <-connection.LostConnection:
		return false
	default:
		return m.browser.Err() == nil
	}
}

func (m *Manager) ready(ctx context.Context) error {
	if m.closed || m.root.Err() != nil {
		return unavailable()
	}
	if m.navigationFailed.Load() {
		return domain.NewError(503, "BROWSER_NAVIGATION_GUARD_FAILED", "Navigation protection failed. Inspect the browser and recover it explicitly before continuing.")
	}
	if m.downloadFailed.Load() {
		return downloadGuardFailure()
	}
	if m.alive() {
		return nil
	}
	if m.started {
		m.signal()
		return unavailable()
	}
	m.started = true
	if err := m.start(ctx); err != nil {
		m.failures = 1
		m.nextRecovery = time.Now().Add(time.Second)
		m.notify()
		return errors.Join(unavailable(), err)
	}
	return nil
}

// start 只恢复标签地址，不重放输入、点击或提交。调用时持有门禁。
func (m *Manager) start(ctx context.Context) error {
	if m.stopBrowser != nil {
		m.stopBrowser()
	}
	if m.stopAllocator != nil {
		m.stopAllocator()
	}
	m.browser = nil
	if err := m.clearDownloadStaging(); err != nil {
		return err
	}
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.ExecPath(m.options.Executable), chromedp.UserDataDir(m.options.ProfileDir), chromedp.WindowSize(ViewportWidth, ViewportHeight), chromedp.Flag("disable-popup-blocking", false), chromedp.Flag("hide-crash-restore-bubble", true))
	if os.Geteuid() == 0 {
		options = append(options, chromedp.NoSandbox)
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(m.root, options...)
	stopAllocator := sync.OnceFunc(cancelAllocator)
	browser, cancelBrowser := chromedp.NewContext(allocator, chromedp.WithErrorf(func(string, ...any) { m.options.Logger.Warn("浏览器协议报告异常") }))
	// 分配失败时 chromedp 的清理函数会消费一次信号；用 Once 防止恢复或并发取消重复等待。
	stopBrowser := sync.OnceFunc(cancelBrowser)
	m.stopAllocator, m.stopBrowser = stopAllocator, stopBrowser
	// 首次分配必须绑定持久上下文；请求结束只能停止启动等待，不能杀死已就绪的浏览器。
	err := initialize(browser, ctx, stopBrowser)
	if err != nil {
		stopBrowser()
		stopAllocator()
		return err
	}
	m.browser = browser
	startup, cancel := operationContext(browser, ctx)
	m.guard = nil
	if m.options.AuthorizeNavigation != nil {
		m.guard = newNavigationGuard(browser, chromedp.FromContext(browser).Browser, m.options.AuthorizeNavigation, m.options.Logger, func() {
			m.navigationFailed.Store(true)
			m.notify()
			go func() { stopBrowser(); m.signal() }()
		}, m.notify)
		if err = m.guard.start(startup); err != nil {
			cancel()
			stopBrowser()
			stopAllocator()
			m.browser = nil
			return err
		}
	}
	_, err = cdp.Call(startup, chromedp.FromContext(browser).Browser, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{Behavior: cdpbrowser.SetDownloadBehaviorBehaviorDeny})
	cancel()
	if err != nil {
		stopBrowser()
		stopAllocator()
		m.browser = nil
		return err
	}
	if err = chromedp.ListenBrowser(browser, func(event any) {
		switch event.(type) {
		case *target.EventTargetCreated, *target.EventTargetDestroyed, *target.EventTargetInfoChanged:
			m.signal()
		}
	}); err != nil {
		stopBrowser()
		stopAllocator()
		m.browser = nil
		return err
	}
	for _, tab := range m.tabs {
		tab.refs = nil
		tab.BlockedNavigation = nil
		tab.Revision++
		if err = m.attach(ctx, tab, ""); err == nil && tab.URL != "about:blank" {
			address, validationErr := NormalizeURL(tab.URL)
			if validationErr != nil {
				err = validationErr
			} else {
				err = m.navigate(ctx, tab, address)
			}
		}
		if err != nil {
			tab.ErrorCode = "BROWSER_RESTORE_FAILED"
			m.options.Logger.Warn("浏览器标签恢复失败", "tab_id", tab.ID, "bot_id", tab.BotID)
		} else {
			tab.ErrorCode = ""
		}
	}
	if m.navigationFailed.Load() {
		return domain.NewError(503, "BROWSER_NAVIGATION_GUARD_FAILED", "Navigation protection failed while restoring tabs.")
	}
	m.failures = 0
	m.options.Logger.Info("持久浏览器已启动", "tabs", len(m.tabs))
	m.notify()
	return nil
}

func operationContext(base, request context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(base, OperationTimeout)
	stop := context.AfterFunc(request, cancel)
	if request.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func initialize(persistent, request context.Context, stop context.CancelFunc) error {
	if err := request.Err(); err != nil {
		return err
	}
	stopRequest := context.AfterFunc(request, stop)
	timer := time.AfterFunc(OperationTimeout, stop)
	err := chromedp.Do(persistent)
	stopRequest()
	timer.Stop()
	if request.Err() != nil {
		return errors.Join(request.Err(), err)
	}
	return err
}

func (m *Manager) attach(request context.Context, tab *entry, id target.ID) error {
	var options []chromedp.ContextOption
	if id != "" {
		options = append(options, chromedp.WithTargetID(id))
	}
	tabContext, cancelTab := chromedp.NewContext(m.browser, options...)
	tab.ctx, tab.stop = tabContext, sync.OnceFunc(cancelTab)
	// 首次 Target.run 同样绑定调用上下文，初始化时不能使用随后会取消的命令上下文。
	if err := initialize(tab.ctx, request, tab.stop); err != nil {
		tab.stop()
		return err
	}
	ctx, cancel := operationContext(tab.ctx, request)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		_, err := cdp.Call(ctx, t, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{Width: ViewportWidth, Height: ViewportHeight, DeviceScaleFactor: 1, Mobile: false})
		return err
	})); err != nil {
		tab.stop()
		return err
	}
	tab.target = chromedp.FromContext(tab.ctx).Target.TargetID
	guard := m.guard
	if guard != nil {
		tree, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, page.GetFrameTree, cdp.Empty{})
		if err != nil {
			tab.stop()
			return err
		}
		if err = guard.bind(tab.Tab, tab.target, tree.FrameTree); err != nil {
			tab.stop()
			return err
		}
	}
	return chromedp.Listen(tab.ctx, func(event any) {
		if guard != nil {
			switch event := event.(type) {
			case *page.EventFrameAttached:
				guard.attached(event.FrameID, event.ParentFrameID)
			case *page.EventFrameDetached:
				guard.detached(event.FrameID)
			}
		}
		switch event.(type) {
		case *page.EventFrameNavigated, *page.EventNavigatedWithinDocument:
			tab.epoch.Add(1)
			m.signal()
		}
	})
}

func (m *Manager) capacity(owner string) error {
	count := 0
	for _, tab := range m.tabs {
		if tab.BotID == owner {
			count++
		}
	}
	if len(m.tabs) >= MaxTabs || count >= MaxOwnerTabs {
		return domain.NewError(409, "BROWSER_TAB_LIMIT", "Close a tab before opening another one.")
	}
	return nil
}

// Open 在共享 profile 内建立归属标签；URL 验证先于启动。被阻断导航保留标签及原因，其他打开失败清理目标。
func (m *Manager) Open(parent context.Context, requested Tab) (result Tab, err error) {
	owner, address := requested.BotID, requested.URL
	address, err = NormalizeURL(address)
	if err != nil {
		return result, err
	}
	if owner == "" || len(owner) > 128 {
		return result, invalid("Provide a bot or user owner.")
	}
	if _, err = ulid.ParseStrict(requested.ID); err != nil || requested.Revision == 0 {
		return result, invalid("Provide a valid tab ID and page revision.")
	}
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err = m.acquire(ctx); err != nil {
		return result, err
	}
	defer m.release()
	if m.tabs[requested.ID] != nil {
		return result, domain.NewError(409, "BROWSER_TAB_EXISTS", "The browser tab already exists.")
	}
	if err = m.capacity(owner); err != nil {
		return result, err
	}
	if err = m.ready(ctx); err != nil {
		return result, err
	}
	tab := &entry{Tab: Tab{ID: requested.ID, BotID: owner, URL: address, Revision: requested.Revision}}
	if err = m.attach(ctx, tab, ""); err != nil {
		m.forgetNavigation(tab.ID)
		return result, err
	}
	if err = m.navigate(ctx, tab, address); err != nil {
		if tab.BlockedNavigation != nil {
			m.tabs[tab.ID] = tab
			m.notify()
			return tab.Tab, err
		}
		m.forgetNavigation(tab.ID)
		tab.stop()
		return result, err
	}
	m.tabs[tab.ID] = tab
	m.notify()
	return tab.Tab, nil
}

func (m *Manager) navigate(request context.Context, tab *entry, address string) error {
	if m.guard != nil {
		m.guard.clear(tab.ID)
	}
	tab.BlockedNavigation = nil
	tab.ErrorCode = ""
	tab.refs = nil
	tab.Revision++
	ctx, cancel := operationContext(tab.ctx, request)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate(address)); err != nil {
		m.syncNavigation(tab)
		if tab.BlockedNavigation != nil {
			return domain.NewError(403, tab.BlockedNavigation.Code, "Navigation was blocked before sending the request. Inspect blocked_navigation and request approval in a new tool call; do not replay a form submission.")
		}
		if request.Err() != nil {
			return request.Err()
		}
		if ctx.Err() != nil {
			return domain.NewError(408, "BROWSER_NAV_TIMEOUT", "Navigation timed out. Take a new snapshot before deciding whether to navigate again.")
		}
		return errors.Join(domain.NewError(502, "BROWSER_NAV_FAILED", "Navigation failed. Inspect the tab before retrying."), err)
	}
	return m.metadata(ctx, tab)
}

func (m *Manager) metadata(ctx context.Context, tab *entry) error {
	address, err := chromedp.Run(ctx, chromedp.Location())
	if err != nil {
		return err
	}
	title, err := chromedp.Run(ctx, chromedp.Title())
	if err != nil {
		return err
	}
	tab.URL = address
	tab.Title = boundedText(title, 512)
	return nil
}

func (m *Manager) withTab(parent context.Context, selected Selection, action func(context.Context, *entry) error) error {
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	tab, ok := m.tabs[selected.ID]
	if !ok {
		return domain.NewError(404, "BROWSER_TAB_NOT_FOUND", "The browser tab does not exist.")
	}
	if tab.BotID != selected.Owner {
		return domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "This tab belongs to another owner.")
	}
	if err := m.ready(ctx); err != nil {
		return err
	}
	if tab.ctx == nil || tab.ctx.Err() != nil {
		return unavailable()
	}
	syncRevision(tab)
	m.syncNavigation(tab)
	if selected.Revision != 0 && selected.Revision != tab.Revision {
		return domain.NewError(409, "BROWSER_TAB_CHANGED", "The page changed. Inspect it again before submitting a new action.")
	}
	operation, stop := operationContext(tab.ctx, ctx)
	defer stop()
	err := action(operation, tab)
	m.syncNavigation(tab)
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	if !m.alive() {
		m.signal()
		return errors.Join(unavailable(), err)
	}
	return err
}

func syncRevision(tab *entry) {
	epoch := tab.epoch.Load()
	if tab.appliedEpoch != epoch {
		tab.appliedEpoch = epoch
		tab.Revision++
		if tab.refsEpoch != epoch {
			tab.refs = nil
		}
	}
}

func (m *Manager) Navigate(ctx context.Context, selected Selection, address string) (Tab, error) {
	address, err := NormalizeURL(address)
	if err != nil {
		return Tab{}, err
	}
	var result Tab
	err = m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		err := m.navigate(ctx, tab, address)
		result = tab.Tab
		m.notify()
		return err
	})
	return result, err
}

// List 返回有界元数据快照，不启动浏览器；空 owner 供已鉴权的用户列出全部标签。
func (m *Manager) List(parent context.Context, owner string) ([]Tab, error) {
	var tabs []Tab
	err := m.Inspect(parent, func(all []Tab) error {
		tabs = make([]Tab, 0, len(all))
		for _, tab := range all {
			if owner == "" || tab.BotID == owner {
				tabs = append(tabs, tab)
			}
		}
		return nil
	})
	return tabs, err
}

// Inspect 在标签锁内读取一致快照；观察者可保存元数据，但不能回调 Manager，避免弹窗落库与关闭同步交错。
func (m *Manager) Inspect(parent context.Context, observe func([]Tab) error) error {
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	tabs := make([]Tab, 0, len(m.tabs))
	for _, tab := range m.tabs {
		syncRevision(tab)
		m.syncNavigation(tab)
		copy := tab.Tab
		if m.started && !m.alive() {
			copy.ErrorCode = "BROWSER_UNAVAILABLE"
		}
		tabs = append(tabs, copy)
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].ID < tabs[j].ID })
	return observe(tabs)
}

func (m *Manager) CloseTab(parent context.Context, selected Selection) error {
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	tab, ok := m.tabs[selected.ID]
	if !ok {
		return domain.NewError(404, "BROWSER_TAB_NOT_FOUND", "The browser tab does not exist.")
	}
	if tab.BotID != selected.Owner {
		return domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "This tab belongs to another owner.")
	}
	syncRevision(tab)
	if selected.Revision != 0 && selected.Revision != tab.Revision {
		return domain.NewError(409, "BROWSER_TAB_CHANGED", "The page changed before closing.")
	}
	if tab.ctx != nil && tab.ctx.Err() == nil && m.alive() {
		operation, cancel := operationContext(tab.ctx, ctx)
		err := chromedp.Cancel(operation)
		cancel()
		if err != nil {
			return err
		}
	}
	if tab.stop != nil {
		tab.stop()
	}
	delete(m.tabs, selected.ID)
	m.forgetNavigation(selected.ID)
	m.notify()
	return nil
}

// Restart 供显式恢复操作重置重试预算；不会重放导致中断的动作。
func (m *Manager) Restart(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	if m.closed || m.root.Err() != nil {
		return unavailable()
	}
	if m.navigationFailed.Load() || m.downloadFailed.Load() {
		if m.stopBrowser != nil {
			m.stopBrowser()
		}
		m.navigationFailed.Store(false)
		m.downloadFailed.Store(false)
	}
	if m.alive() {
		return nil
	}
	m.started = true
	m.failures = 0
	return m.start(ctx)
}

// Close 先让 Chromium 刷写 profile 再回收进程；可重复调用，调用方可限制等待时长。
func (m *Manager) Close(ctx context.Context) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	if m.closed {
		m.release()
		return nil
	}
	m.closed = true
	var err error
	if m.alive() {
		closeContext, cancel := operationContext(m.browser, ctx)
		err = chromedp.Cancel(closeContext)
		cancel()
	}
	m.stop()
	if m.stopBrowser != nil {
		m.stopBrowser()
	}
	if m.stopAllocator != nil {
		m.stopAllocator()
	}
	m.release()
	m.wg.Wait()
	return err
}

func (m *Manager) supervise() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.root.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
		ctx, cancel := context.WithTimeout(m.root, OperationTimeout)
		if err := m.acquire(ctx); err != nil {
			cancel()
			continue
		}
		if !m.closed && m.started {
			if !m.alive() && !m.navigationFailed.Load() && !m.downloadFailed.Load() && m.failures < maxRecoveries && !time.Now().Before(m.nextRecovery) {
				if err := m.start(ctx); err != nil {
					m.failures++
					m.nextRecovery = time.Now().Add(time.Second * time.Duration(1<<m.failures))
					m.options.Logger.Error("持久浏览器恢复失败", "attempt", m.failures)
					m.notify()
				}
			}
			if m.alive() {
				if err := m.reconcile(ctx); err != nil && m.root.Err() == nil {
					m.options.Logger.Warn("浏览器标签同步失败")
				}
			}
		}
		m.release()
		cancel()
	}
}

// reconcile 按 opener 继承弹出标签归属，拒绝无归属页面并限制标签总数。
func (m *Manager) reconcile(ctx context.Context) error {
	result, err := cdp.Call(ctx, chromedp.FromContext(m.browser).Browser, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return err
	}
	known := map[target.ID]*entry{}
	for _, tab := range m.tabs {
		known[tab.target] = tab
	}
	alive := map[target.ID]bool{}
	rootTarget := chromedp.FromContext(m.browser).Target.TargetID
	for _, info := range result.TargetInfos {
		if info.Type != "page" || info.TargetID == rootTarget {
			continue
		}
		alive[info.TargetID] = true
		if tab := known[info.TargetID]; tab != nil {
			if tab.URL != info.URL || tab.Title != boundedText(info.Title, 512) {
				tab.URL, tab.Title = info.URL, boundedText(info.Title, 512)
				m.notify()
			}
			continue
		}
		parent := known[info.OpenerID]
		if parent == nil || m.capacity(parent.BotID) != nil {
			if _, err := cdp.Call(ctx, chromedp.FromContext(m.browser).Browser, target.CloseTarget, target.CloseTargetParams{TargetID: info.TargetID}); err != nil {
				return err
			}
			continue
		}
		tab := &entry{Tab: Tab{ID: ids.New(), BotID: parent.BotID, URL: info.URL, Title: boundedText(info.Title, 512), Revision: 1}}
		if m.options.PersistPopup != nil {
			if err := m.options.PersistPopup(ctx, parent.ID, tab.Tab); err != nil {
				_, closeErr := cdp.Call(ctx, chromedp.FromContext(m.browser).Browser, target.CloseTarget, target.CloseTargetParams{TargetID: info.TargetID})
				return errors.Join(err, closeErr)
			}
			defer m.notify()
		}
		if err := m.attach(ctx, tab, info.TargetID); err != nil {
			return err
		}
		m.tabs[tab.ID] = tab
		known[info.TargetID] = tab
		m.notify()
	}
	for id, tab := range m.tabs {
		if !alive[tab.target] && tab.ErrorCode != "BROWSER_RESTORE_FAILED" {
			tab.stop()
			delete(m.tabs, id)
			m.forgetNavigation(id)
			m.notify()
		}
	}
	return nil
}
