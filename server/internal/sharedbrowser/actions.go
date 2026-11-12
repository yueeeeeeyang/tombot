package sharedbrowser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/computer"
)

var ErrNavigation = errors.New("浏览器导航未完成或没有对应历史记录")

// Execute 发送一次完整动作，遇到页面对话框先返回等待状态，供同一 Agent 下一步答复。
// 相同末次命令只读取当前回执；等待不表示动作完成，其他新动作须等原动作结束。
func (service *Service) Execute(ctx context.Context, address Address, command Command) (Window, error) {
	if err := command.Validate(service.options.Width, service.options.Height); err != nil {
		return Window{}, err
	}
	return service.execute(ctx, address, command, func(_ context.Context, item *entry) (func(context.Context) error, error) {
		return func(ctx context.Context) error { return service.command(ctx, item, command) }, nil
	})
}

// execute 在窗口门禁内先校验和准备，再消费动作序号；准备失败不得伪装成已执行的网页动作。
func (service *Service) execute(ctx context.Context, address Address, command Command, prepare func(context.Context, *entry) (func(context.Context) error, error)) (Window, error) {
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	item, err := service.own(bounded, address)
	if err != nil {
		return Window{}, err
	}
	interrupt := context.AfterFunc(item.life, cancel)
	defer interrupt()
	service.mu.Lock()
	window, failure, handled := commandReceipt(item, command)
	service.mu.Unlock()
	if handled {
		return window, failure
	}
	select {
	case item.gate <- struct{}{}:
	default:
		return Window{}, ErrBusy
	}
	// 在发出任何 CDP 前再次检查授权与门禁；关闭与并发请求都可能改变刚读到的状态。
	if err := service.authorize(bounded, address.Session); err != nil {
		<-item.gate
		return Window{}, err
	}
	service.mu.Lock()
	window, failure, handled = commandReceipt(item, command)
	if handled || service.closed || service.failed || item.life.Err() != nil || bounded.Err() != nil {
		service.mu.Unlock()
		<-item.gate
		if handled {
			return window, failure
		}
		return Window{}, ErrDenied
	}
	service.mu.Unlock()
	action, err := prepare(bounded, item)
	if err == nil {
		err = service.authorize(bounded, address.Session)
	}
	service.mu.Lock()
	if err != nil || service.closed || service.failed || item.life.Err() != nil || bounded.Err() != nil {
		service.mu.Unlock()
		<-item.gate
		if err != nil {
			return Window{}, err
		}
		return Window{}, ErrDenied
	}
	run := newCommandRun(item.life, service.options.CallTimeout, service.options.DialogTimeout)
	item.last, item.lastError, item.active = command, ErrUnknown, run
	item.window.Sequence, item.window.CommandState = command.Sequence, "running"
	service.commands.Add(1)
	service.mu.Unlock()
	stopCaller := context.AfterFunc(bounded, run.cancel)
	defer stopCaller()
	go service.runCommand(item, action, run)
	for {
		select {
		case <-bounded.Done():
			run.cancel()
			return Window{}, ErrUnknown
		case <-run.done:
			if service.authorize(bounded, address.Session) != nil {
				return Window{}, ErrUnknown
			}
			return run.result, run.failure
		case <-run.changed:
			service.mu.Lock()
			window = item.window
			waiting := item.active == run && window.Dialog.ID != 0
			service.mu.Unlock()
			if !waiting {
				continue
			}
			if service.authorize(bounded, address.Session) != nil || !stopCaller() || bounded.Err() != nil || run.ctx.Err() != nil {
				run.cancel()
				return Window{}, ErrUnknown
			}
			return window, nil
		}
	}
}

// commandReceipt 在元数据锁内检查顺序与在途状态，不把等待对话框等同于已完成。
func commandReceipt(item *entry, command Command) (Window, error, bool) {
	if item.life == nil || item.life.Err() != nil {
		return Window{}, ErrDenied, true
	}
	if command.Sequence == item.last.Sequence {
		if command != item.last {
			return Window{}, ErrConflict, true
		}
		if item.active != nil {
			if item.active.ctx.Err() != nil {
				return Window{}, ErrUnknown, true
			}
			return item.window, nil, true
		}
		return item.window, item.lastError, true
	}
	if command.Sequence < item.last.Sequence {
		return Window{}, ErrConflict, true
	}
	if item.window.State != "ready" {
		return Window{}, ErrUnknown, true
	}
	if item.window.Dialog.ID != 0 {
		return Window{}, ErrDialog, true
	}
	if item.active != nil {
		return Window{}, ErrBusy, true
	}
	return Window{}, nil, false
}

func (service *Service) runCommand(item *entry, action func(context.Context) error, run *commandRun) {
	defer service.commands.Done()
	err := action(run.ctx)
	if err == nil || errors.Is(err, ErrNavigation) {
		if service.authorize(run.ctx, item.session) != nil {
			err = ErrUnknown
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	defer func() {
		run.result, run.failure = item.window, item.lastError
		run.finish()
		<-item.gate
		close(run.done)
	}()
	item.active = nil
	if run.ctx.Err() != nil || err != nil && !errors.Is(err, ErrNavigation) {
		item.lastError, item.window.CommandState = ErrUnknown, "unknown"
		if item.window.State == "ready" {
			item.window.State = "unknown"
		}
		return
	}
	item.lastError, item.window.CommandState = err, "completed"
	if errors.Is(err, ErrNavigation) {
		item.window.CommandState = "navigation_failed"
	}
}

// windowBusy 在进入可能等待脚本的 CDP 读取前拒绝在途动作，调用者可继续读取列表及对话框。
func (service *Service) windowBusy(item *entry) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if item.window.State != "ready" {
		return ErrUnknown
	}
	if item.window.Dialog.ID != 0 {
		return ErrDialog
	}
	if item.active != nil {
		return ErrBusy
	}
	return nil
}

func (service *Service) command(ctx context.Context, item *entry, command Command) error {
	tab := chromedp.FromContext(item.ctx).Target
	switch command.Kind {
	case "navigate":
		result, err := cdp.Call(ctx, tab, page.Navigate, page.NavigateParams{URL: command.URL})
		if err != nil {
			return err
		}
		if result.ErrorText != "" {
			return ErrNavigation
		}
	case "reload":
		_, err := cdp.Call(ctx, tab, page.Reload, page.ReloadParams{})
		return err
	case "back", "forward":
		history, err := cdp.Call(ctx, tab, page.GetNavigationHistory, cdp.Empty{})
		if err != nil {
			return err
		}
		index := history.CurrentIndex - 1
		if command.Kind == "forward" {
			index = history.CurrentIndex + 1
		}
		if index < 0 || index >= int64(len(history.Entries)) {
			return ErrNavigation
		}
		_, err = cdp.Call(ctx, tab, page.NavigateToHistoryEntry, page.NavigateToHistoryEntryParams{EntryID: history.Entries[index].ID})
		return err
	case "type":
		_, err := cdp.Call(ctx, tab, input.InsertText, input.InsertTextParams{Text: command.Text})
		return err
	case "key":
		key := browserKey(command)
		if _, err := cdp.Call(ctx, tab, input.DispatchKeyEvent, key); err != nil {
			return err
		}
		if command.Key == "Enter" && command.Modifiers&7 == 0 {
			// Enter 的字符事件负责按钮激活与表单提交，只有按下/抬起会漏掉页面默认行为。
			character := key
			character.Type, character.Text, character.UnmodifiedText = input.DispatchKeyEventTypeChar, "\r", "\r"
			if _, err := cdp.Call(ctx, tab, input.DispatchKeyEvent, character); err != nil {
				return err
			}
		}
		key.Type = input.DispatchKeyEventTypeKeyUp
		_, err := cdp.Call(ctx, tab, input.DispatchKeyEvent, key)
		return err
	case "move", "click", "double_click", "scroll":
		pointer := input.DispatchMouseEventParams{Type: input.DispatchMouseEventTypeMouseMoved, X: float64(command.X), Y: float64(command.Y)}
		if command.Kind == "move" {
			_, err := cdp.Call(ctx, tab, input.DispatchMouseEvent, pointer)
			return err
		}
		if command.Kind == "scroll" {
			pointer.Type, pointer.DeltaX, pointer.DeltaY = input.DispatchMouseEventTypeMouseWheel, float64(command.DeltaX), float64(command.DeltaY)
			_, err := cdp.Call(ctx, tab, input.DispatchMouseEvent, pointer)
			return err
		}
		count := int64(1)
		if command.Kind == "double_click" {
			count = 2
		}
		for click := int64(1); click <= count; click++ {
			pointer.Type, pointer.Button, pointer.Buttons, pointer.ClickCount = input.DispatchMouseEventTypeMousePressed, input.MouseButtonLeft, 1, click
			if _, err := cdp.Call(ctx, tab, input.DispatchMouseEvent, pointer); err != nil {
				return err
			}
			pointer.Type, pointer.Buttons = input.DispatchMouseEventTypeMouseReleased, 0
			if _, err := cdp.Call(ctx, tab, input.DispatchMouseEvent, pointer); err != nil {
				return err
			}
		}
	default:
		return ErrInvalid
	}
	return nil
}

// browserKey 保留物理键与修饰键的语义，普通文本仍统一通过 InsertText 输入。
func browserKey(command Command) input.DispatchKeyEventParams {
	key := input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyDown, Key: command.Key, Code: command.Key, WindowsVirtualKeyCode: keyCodes[command.Key], Modifiers: command.Modifiers}
	if len(command.Key) == 1 {
		key.Code = "Key" + command.Key
		if command.Modifiers&8 == 0 {
			key.Key = strings.ToLower(command.Key)
		}
	}
	return key
}

// Screenshot 仅捕获所属页面的当前视口，验证真实 PNG 与固定尺寸，不接受外部 target 或浏览器全局截图。
func (service *Service) Screenshot(ctx context.Context, address Address) ([]byte, error) {
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	item, err := service.own(bounded, address)
	if err != nil {
		return nil, err
	}
	if err := service.windowBusy(item); err != nil {
		return nil, err
	}
	interrupt := context.AfterFunc(item.life, cancel)
	defer interrupt()
	if err := acquire(bounded, item.gate); err != nil {
		return nil, err
	}
	defer func() { <-item.gate }()
	if item.life.Err() != nil {
		return nil, ErrDenied
	}
	if err := service.authorize(bounded, address.Session); err != nil {
		return nil, err
	}
	service.mu.Lock()
	ready := item.window.State == "ready"
	service.mu.Unlock()
	if !ready {
		return nil, ErrUnknown
	}
	positive, negative := true, false
	pixels, err := captureActivePage(bounded, func() error {
		if err := service.authorize(bounded, address.Session); err != nil {
			return err
		}
		return service.windowBusy(item)
	}, func() ([]byte, error) {
		result, err := cdp.Call(bounded, chromedp.FromContext(item.ctx).Target, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng, FromSurface: &positive, CaptureBeyondViewport: &negative})
		if err != nil {
			return nil, err
		}
		return result.Data, nil
	})
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if len(pixels) == 0 || len(pixels) > 4<<20 {
		return nil, fmt.Errorf("截图字节数不符: %w", ErrUnavailable)
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(pixels))
	if err != nil || configuration.Width != service.options.Width || configuration.Height != service.options.Height {
		return nil, fmt.Errorf("截图尺寸不符（%d×%d）: %w", configuration.Width, configuration.Height, ErrUnavailable)
	}
	if _, err := png.Decode(bytes.NewReader(pixels)); err != nil {
		return nil, ErrUnavailable
	}
	if err := service.authorize(bounded, address.Session); err != nil {
		return nil, err
	}
	return pixels, nil
}

// CloseWindow 撤销本窗口在途动作并确认页面已消失；关闭结果不明会封闭共享浏览器，不能遗留无主页面。
func (service *Service) CloseWindow(ctx context.Context, address Address) error {
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	item, err := service.own(bounded, address)
	if err != nil {
		return err
	}
	return service.closeWindow(bounded, item)
}
func (service *Service) closeWindow(ctx context.Context, item *entry) error {
	service.mu.Lock()
	if item.window.State == "closed" {
		service.mu.Unlock()
		return service.releaseUploads(item)
	}
	item.window.State = "closing"
	item.revoke()
	service.mu.Unlock()
	if err := acquire(ctx, item.gate); err != nil {
		service.fail()
		return ErrUnknown
	}
	defer func() { <-item.gate }()
	if err := acquire(ctx, item.dialogGate); err != nil {
		service.fail()
		return ErrUnknown
	}
	defer func() { <-item.dialogGate }()
	if err := service.closeTarget(ctx, item.target); err != nil {
		service.fail()
		return err
	}
	service.mu.Lock()
	item.window.State = "closed"
	delete(service.targets, item.target)
	service.mu.Unlock()
	item.stop()
	return service.releaseUploads(item)
}

// ReleaseSession 是受信监督器的撤销入口，即使租约已失效也按原完整身份关闭全部所属页面；不触及其他桌面。
func (service *Service) ReleaseSession(ctx context.Context, session computer.ExecutionSession) error {
	if session.Validate() != nil || session.UserID != service.options.UserID || session.ComputerID != service.options.ComputerID || session.Generation != service.options.Generation {
		return ErrDenied
	}
	bounded, cancel := context.WithTimeout(ctx, service.options.ShutdownTimeout)
	defer cancel()
	if err := acquire(bounded, service.creation); err != nil {
		service.fail()
		return err
	}
	defer func() { <-service.creation }()
	service.mu.Lock()
	var owned []*entry
	for _, item := range service.windows {
		if item.session == session {
			owned = append(owned, item)
			if item.window.State != "closed" {
				item.window.State = "closing"
			}
			item.revoke()
		}
	}
	service.mu.Unlock()
	for _, item := range owned {
		if err := service.closeWindow(bounded, item); err != nil {
			return err
		}
	}
	service.mu.Lock()
	for _, item := range owned {
		delete(service.windows, item.window.ID)
	}
	service.mu.Unlock()
	return nil
}
