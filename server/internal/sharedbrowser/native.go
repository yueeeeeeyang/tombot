package sharedbrowser

import (
	"context"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"io.yueyang/tom/internal/computer"
)

// NativeDesktop 由容器代理提供；只呈现已授权的原生窗口，截图与输入均使用 Bot 桌面坐标。
type NativeDesktop interface {
	Bind(context.Context, computer.ExecutionSession, int64, [4]int) error
	Execute(context.Context, computer.ExecutionSession, int64, Command) error
	Screenshot(context.Context, computer.ExecutionSession, int64) ([]byte, error)
	Release(context.Context, computer.ExecutionSession) error
	HumanControl(context.Context, computer.ExecutionSession, bool) error
}

// HumanControl 是监督器的交接屏障，原生服务确认先前输入已结束后才允许用户输入。
func (service *Service) HumanControl(ctx context.Context, session computer.ExecutionSession, paused bool) error {
	if session.Validate() != nil || session.UserID != service.options.UserID || session.ComputerID != service.options.ComputerID || session.Generation != service.options.Generation {
		return ErrDenied
	}
	return service.options.Native.HumanControl(ctx, session, paused)
}

type nativeWindow struct {
	session computer.ExecutionSession
	bounds  [4]int
	gate    chan struct{}
}

// withNativeTarget 串行切换同一个原生窗口的标签，保证截图和输入都落在请求的页面上。
func (service *Service) withNativeTarget(ctx context.Context, item *entry, action func(context.Context) error) error {
	if err := acquire(ctx, item.nativeGate); err != nil {
		return err
	}
	defer func() { <-item.nativeGate }()
	if _, err := cdp.Call(ctx, service.browser, target.ActivateTarget, target.ActivateTargetParams{TargetID: item.target}); err != nil {
		return err
	}
	return action(ctx)
}

// bindNative 在创建门禁内给新原生窗口分配临时停靠位置，避免根据可被网页伪造的标题判断归属。
func (service *Service) bindNative(ctx context.Context, session computer.ExecutionSession, id target.ID) (int64, error) {
	result, err := cdp.Call(ctx, service.browser, browser.GetWindowForTarget, browser.GetWindowForTargetParams{TargetID: id})
	if err != nil || result.WindowID <= 0 {
		return 0, ErrUnavailable
	}
	windowID := int64(result.WindowID)
	if existing, ok := service.nativeWindows[windowID]; ok {
		if existing.session != session {
			return 0, ErrDenied
		}
		return windowID, nil
	}
	service.nativeSequence++
	if service.nativeSequence > 4096 {
		return 0, ErrCapacity
	}
	left, top := int64(4096+service.nativeSequence%128), int64(4096+service.nativeSequence/128)
	width, height := int64(1200), int64(740)
	_, err = cdp.Call(ctx, service.browser, browser.SetWindowBounds, browser.SetWindowBoundsParams{WindowID: result.WindowID,
		Bounds: &browser.Bounds{Left: &left, Top: &top, Width: width, Height: height, WindowState: browser.WindowStateNormal}})
	if err != nil {
		return 0, err
	}
	bounds := [4]int{int(left), int(top), int(width), int(height)}
	if err = service.options.Native.Bind(ctx, session, windowID, bounds); err != nil {
		return 0, err
	}
	service.nativeWindows[windowID] = nativeWindow{session: session, bounds: bounds, gate: make(chan struct{}, 1)}
	return windowID, nil
}
