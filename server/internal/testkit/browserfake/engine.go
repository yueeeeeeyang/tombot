// Package browserfake 为服务与 HTTP 集成测试提供可控浏览器进程，真实协议由 browser 包的实机测试覆盖。
package browserfake

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"strings"
	"sync"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
)

type Engine struct {
	mu         sync.Mutex
	tabs       map[string]browser.Tab
	wake       chan struct{}
	options    browser.Options
	operations []string
	failure    error
	actionHook func(context.Context) error
}

func New(options browser.Options) *Engine {
	return &Engine{tabs: map[string]browser.Tab{}, wake: make(chan struct{}, 1), options: options}
}
func (e *Engine) SetFailure(err error) { e.mu.Lock(); defer e.mu.Unlock(); e.failure = err }
func (e *Engine) SetActionHook(hook func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actionHook = hook
}
func (e *Engine) Operations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.operations...)
}
func (e *Engine) Changed() <-chan struct{} { return e.wake }
func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) Restore(ctx context.Context, tabs []browser.Tab) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, tab := range tabs {
		e.tabs[tab.ID] = tab
	}
	return ctx.Err()
}
func (e *Engine) Open(ctx context.Context, tab browser.Tab) (browser.Tab, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.operations = append(e.operations, "open")
	if e.failure != nil {
		return browser.Tab{}, e.failure
	}
	if err := ctx.Err(); err != nil {
		return browser.Tab{}, err
	}
	tab.Revision++
	tab.Title = "测试页面"
	e.tabs[tab.ID] = tab
	e.notify()
	return tab, nil
}
func (e *Engine) Inspect(ctx context.Context, observe func([]browser.Tab) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tabs := make([]browser.Tab, 0, len(e.tabs))
	for _, tab := range e.tabs {
		tabs = append(tabs, tab)
	}
	return observe(tabs)
}
func (e *Engine) List(ctx context.Context, owner string) ([]browser.Tab, error) {
	var tabs []browser.Tab
	err := e.Inspect(ctx, func(all []browser.Tab) error {
		for _, tab := range all {
			if owner == "" || owner == tab.BotID {
				tabs = append(tabs, tab)
			}
		}
		return nil
	})
	return tabs, err
}
func (e *Engine) Status(ctx context.Context) (browser.Status, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return browser.Status{State: "running", Tabs: len(e.tabs)}, ctx.Err()
}
func (e *Engine) Restart(ctx context.Context) error { return ctx.Err() }
func (e *Engine) Close(ctx context.Context) error   { return ctx.Err() }
func (e *Engine) action(ctx context.Context, selected browser.Selection, name string) (browser.Tab, error) {
	e.operations = append(e.operations, name)
	tab, exists := e.tabs[selected.ID]
	if !exists {
		return tab, domain.NewError(404, "BROWSER_TAB_NOT_FOUND", "The tab is absent.")
	}
	if tab.BotID != selected.Owner {
		return tab, domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "Incorrect owner.")
	}
	if selected.Revision != 0 && selected.Revision != tab.Revision {
		return tab, domain.NewError(409, "BROWSER_TAB_CHANGED", "The tab changed.")
	}
	if e.failure != nil {
		return tab, e.failure
	}
	if e.actionHook != nil {
		if err := e.actionHook(ctx); err != nil {
			return tab, err
		}
	}
	if err := ctx.Err(); err != nil {
		return tab, err
	}
	tab.Revision++
	e.tabs[tab.ID] = tab
	e.notify()
	return tab, nil
}
func (e *Engine) Navigate(ctx context.Context, selected browser.Selection, url string) (browser.Tab, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tab, err := e.action(ctx, selected, "navigate")
	if err == nil {
		tab.URL = url
		e.tabs[tab.ID] = tab
	}
	return tab, err
}
func (e *Engine) CloseTab(ctx context.Context, selected browser.Selection) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "close")
	if err == nil {
		delete(e.tabs, selected.ID)
	}
	return err
}
func (e *Engine) Click(ctx context.Context, selected browser.Selection, ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "click")
	return err
}
func (e *Engine) Type(ctx context.Context, selected browser.Selection, ref, text string, clear bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "type")
	return err
}
func (e *Engine) Key(ctx context.Context, selected browser.Selection, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "key")
	return err
}
func (e *Engine) Scroll(ctx context.Context, selected browser.Selection, x, y float64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "scroll")
	return err
}

func (e *Engine) Download(ctx context.Context, selected browser.Selection, input browser.DownloadInput, consume func(context.Context, string, io.Reader) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.action(ctx, selected, "download"); err != nil {
		return err
	}
	return consume(ctx, "report.txt", strings.NewReader("测试下载内容"))
}
func (e *Engine) Snapshot(ctx context.Context, selected browser.Selection) (browser.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return browser.Snapshot{Tab: e.tabs[selected.ID], Text: "[ref=e1] button \"继续\""}, ctx.Err()
}
func (e *Engine) CaptureFrame(ctx context.Context, selected browser.Selection, quality int) (browser.ScreenFrame, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failure != nil {
		return browser.ScreenFrame{}, e.failure
	}
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewGray(image.Rect(0, 0, browser.ViewportWidth, browser.ViewportHeight)), nil); err != nil {
		return browser.ScreenFrame{}, err
	}
	return browser.ScreenFrame{Tab: e.tabs[selected.ID], JPEG: picture.Bytes()}, ctx.Err()
}

func (e *Engine) ClickPoint(ctx context.Context, selected browser.Selection, x, y float64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "click_at")
	return err
}

func (e *Engine) InputText(ctx context.Context, selected browser.Selection, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.action(ctx, selected, "input")
	return err
}

// Popup 允许测试卡在落库与接管之间，验证业务同步必须等待同一运行时锁。
func (e *Engine) Popup(ctx context.Context, parent string, tab browser.Tab, beforeAdopt func()) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.notify()
	if err := e.options.PersistPopup(ctx, parent, tab); err != nil {
		return err
	}
	if beforeAdopt != nil {
		beforeAdopt()
	}
	e.tabs[tab.ID] = tab
	return nil
}
func (e *Engine) Change(id, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tab := e.tabs[id]
	tab.URL = url
	tab.Revision++
	e.tabs[id] = tab
	e.notify()
}
