package browser

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/domain"
)

const navigationQueueLimit = 128
const navigationFrameLimit = 2048
const navigationDecisionTimeout = 5 * time.Second

// Navigation 在请求发送前交给授权层；新弹窗尚未登记时使用打开它的标签身份。
type Navigation struct {
	TabID  string
	Owner  string
	URL    string
	Method string
}

type BlockedNavigation struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Code   string `json:"code"`
}

type navigationOwner struct{ tabID, owner string }

// navigationGuard 独立处理协议暂停事件，不获取 Manager 命令锁；授权回调也不能回调 Manager。
type navigationGuard struct {
	ctx       context.Context
	session   cdp.Session
	authorize func(context.Context, Navigation) error
	logger    *slog.Logger
	fatal     func()
	changed   func()
	once      sync.Once
	mu        sync.Mutex
	frames    map[cdp.FrameID]navigationOwner
	pages     map[target.ID]navigationOwner
	blocked   map[string]*BlockedNavigation
	requests  chan *fetch.EventRequestPaused
}

func newNavigationGuard(ctx context.Context, session *chromedp.Browser, authorize func(context.Context, Navigation) error, logger *slog.Logger, stop, changed func()) *navigationGuard {
	return &navigationGuard{ctx: ctx, session: session, authorize: authorize, logger: logger, fatal: stop, changed: changed, frames: map[cdp.FrameID]navigationOwner{}, pages: map[target.ID]navigationOwner{}, blocked: map[string]*BlockedNavigation{}, requests: make(chan *fetch.EventRequestPaused, navigationQueueLimit)}
}

func (g *navigationGuard) start(ctx context.Context) error {
	if err := chromedp.ListenBrowser(g.ctx, func(event any) {
		if paused, ok := event.(*fetch.EventRequestPaused); ok {
			select {
			case g.requests <- paused:
			case <-g.ctx.Done():
			default:
				g.fail("BROWSER_NAVIGATION_OVERFLOW")
			}
		}
	}); err != nil {
		return err
	}
	go g.run()
	_, err := cdp.Call(ctx, g.session, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*", ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest}}})
	return err
}

func (g *navigationGuard) fail(code string) {
	if g.ctx.Err() != nil {
		return
	}
	g.once.Do(func() {
		g.logger.Error("浏览器导航保护失败，正在停止进程", "component", "browser", "error_code", code)
		g.fatal()
	})
}

func (g *navigationGuard) run() {
	for {
		select {
		case <-g.ctx.Done():
			return
		case request := <-g.requests:
			ctx, cancel := context.WithTimeout(g.ctx, navigationDecisionTimeout)
			err := g.handle(ctx, request)
			cancel()
			if err != nil {
				g.fail("BROWSER_NAVIGATION_GUARD_FAILED")
				return
			}
		}
	}
}

func (g *navigationGuard) lookup(frameID cdp.FrameID) (navigationOwner, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	owner, ok := g.frames[frameID]
	return owner, ok
}

// resolve 利用主文档身份或弹窗 opener 继承归属，未确认归属的文档请求一律拒绝。
func (g *navigationGuard) resolve(ctx context.Context, frameID cdp.FrameID) (navigationOwner, error) {
	if owner, ok := g.lookup(frameID); ok {
		return owner, nil
	}
	info, err := cdp.Call(ctx, g.session, target.GetTargetInfo, target.GetTargetInfoParams{TargetID: target.ID(frameID)})
	if err != nil {
		return navigationOwner{}, err
	}
	if info.TargetInfo == nil {
		return navigationOwner{}, domain.NewError(403, "BROWSER_NAVIGATION_OWNER_UNKNOWN", "The navigation owner is unknown.")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	owner, ok := g.pages[info.TargetInfo.OpenerID]
	if !ok && info.TargetInfo.ParentFrameID != "" {
		owner, ok = g.frames[info.TargetInfo.ParentFrameID]
	}
	if !ok || len(g.frames) >= navigationFrameLimit {
		return navigationOwner{}, domain.NewError(403, "BROWSER_NAVIGATION_OWNER_UNKNOWN", "The navigation owner is unknown.")
	}
	g.frames[frameID] = owner
	return owner, nil
}

func (g *navigationGuard) handle(ctx context.Context, paused *fetch.EventRequestPaused) error {
	if paused.Request == nil {
		return errors.New("导航暂停事件缺少请求")
	}
	owner, ownerErr := g.resolve(ctx, paused.FrameID)
	decisionErr := ownerErr
	address := ""
	if decisionErr == nil {
		if address, decisionErr = NormalizeURL(paused.Request.URL); decisionErr == nil {
			decisionErr = g.authorize(ctx, Navigation{TabID: owner.tabID, Owner: owner.owner, URL: address, Method: paused.Request.Method})
		}
	}
	if decisionErr == nil {
		_, err := cdp.Call(ctx, g.session, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: paused.RequestID})
		return g.completed(err)
	}
	code := "BROWSER_NAVIGATION_DENIED"
	var public *domain.Error
	if errors.As(decisionErr, &public) {
		code = public.Code
	} else if ownerErr == nil {
		code = "BROWSER_NAVIGATION_POLICY_FAILED"
	}
	if owner.tabID != "" {
		blocked := &BlockedNavigation{URL: address, Method: boundedText(paused.Request.Method, 16), Code: code}
		g.mu.Lock()
		g.blocked[owner.tabID] = blocked
		g.mu.Unlock()
		g.changed()
	}
	_, err := cdp.Call(ctx, g.session, fetch.FailRequest, fetch.FailRequestParams{RequestID: paused.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
	err = g.completed(err)
	if code == "BROWSER_NAVIGATION_POLICY_FAILED" {
		return errors.Join(decisionErr, err)
	}
	return err
}

// completed 只处理浏览器已经删除的拦截任务；其他协议错误必须停止保护层，不能继续放行。
func (g *navigationGuard) completed(err error) error {
	var protocol *cdproto.Error
	if errors.As(err, &protocol) && protocol.Code == -32602 && protocol.Message == "Invalid InterceptionId." {
		g.logger.Debug("导航请求已取消，拦截任务不存在", "component", "browser")
		return nil
	}
	return err
}

func (g *navigationGuard) bind(tab Tab, id target.ID, tree *page.FrameTree) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	owner := navigationOwner{tabID: tab.ID, owner: tab.BotID}
	g.pages[id] = owner
	var visit func(*page.FrameTree) error
	visit = func(tree *page.FrameTree) error {
		if tree == nil || tree.Frame == nil {
			return errors.New("浏览器缺少主文档身份")
		}
		if len(g.frames) >= navigationFrameLimit {
			return errors.New("浏览器文档数量超出限制")
		}
		g.frames[tree.Frame.ID] = owner
		for _, child := range tree.ChildFrames {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(tree)
}

func (g *navigationGuard) attached(frame, parent cdp.FrameID) {
	g.mu.Lock()
	owner, ok := g.frames[parent]
	full := len(g.frames) >= navigationFrameLimit
	if ok && !full {
		g.frames[frame] = owner
	}
	g.mu.Unlock()
	if full {
		g.fail("BROWSER_NAVIGATION_FRAME_LIMIT")
	}
}

func (g *navigationGuard) detached(frame cdp.FrameID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.frames, frame)
}
func (g *navigationGuard) unbind(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for frame, owner := range g.frames {
		if owner.tabID == id {
			delete(g.frames, frame)
		}
	}
	for page, owner := range g.pages {
		if owner.tabID == id {
			delete(g.pages, page)
		}
	}
	delete(g.blocked, id)
}
func (g *navigationGuard) clear(id string) { g.mu.Lock(); defer g.mu.Unlock(); delete(g.blocked, id) }
func (g *navigationGuard) denied(id string) *BlockedNavigation {
	g.mu.Lock()
	defer g.mu.Unlock()
	if blocked := g.blocked[id]; blocked != nil {
		copy := *blocked
		return &copy
	}
	return nil
}

func (m *Manager) syncNavigation(tab *entry) {
	if m.guard == nil {
		return
	}
	blocked := m.guard.denied(tab.ID)
	if blocked != nil && (tab.BlockedNavigation == nil || *tab.BlockedNavigation != *blocked) {
		tab.Revision++
		tab.refs = nil
	}
	tab.BlockedNavigation = blocked
	if blocked != nil {
		tab.ErrorCode = blocked.Code
	}
}

func (m *Manager) forgetNavigation(id string) {
	if m.guard != nil {
		m.guard.unbind(id)
	}
}
