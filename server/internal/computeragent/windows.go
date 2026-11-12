package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
)

// WindowSession 属于一个完整执行会话；撤销立即封闭输入，关闭须确认入口与所属页面全部回收。
type WindowSession interface {
	UseBrowser(context.Context, computer.ExecutionSession, func(context.Context, sharedbrowser.Browser) error) error
	Revoke()
	Close(context.Context) error
}

type browserScope interface {
	BrowserSession(computer.ExecutionSession) (WindowSession, error)
}

// withBrowser 只在桌面锁内取得原实例绑定，网页动作在锁外执行，允许续约和对话框回复并行。
// 回调不能保留借出的浏览器；完整会话、原实例及撤销由 WindowSession 在调用前后复验。
func (controller *Controller) withBrowser(ctx context.Context, session computer.ExecutionSession, action func(context.Context, sharedbrowser.Browser) error) error {
	if action == nil {
		return sharedbrowser.ErrInvalid
	}
	bounded, leave, err := controller.begin(ctx)
	if err != nil {
		return err
	}
	defer leave()
	var windows WindowSession
	err = controller.withScope(bounded, session, func(_ context.Context, guard *desktopGuard) error {
		scope, ok := guard.scope.(browserScope)
		if !ok {
			return errors.New("当前电脑代理没有会话浏览器能力")
		}
		var err error
		windows, err = scope.BrowserSession(session)
		if err != nil {
			return err
		}
		if windows == nil {
			return errors.New("当前电脑代理缺少原浏览器会话绑定")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return windows.UseBrowser(bounded, session, action)
}

// BrowserWindows 通过原实例返回完整执行会话所属页面，不接受客户端指定 Chrome 或调试地址。
func (controller *Controller) BrowserWindows(ctx context.Context, session computer.ExecutionSession) (result []sharedbrowser.Window, failure error) {
	failure = controller.withBrowser(ctx, session, func(ctx context.Context, browser sharedbrowser.Browser) (err error) {
		result, err = browser.List(ctx, session)
		return err
	})
	if failure != nil {
		result = nil
	}
	return result, failure
}

// OpenBrowserWindow 使用调用方的稳定窗口标识创建页面；授权丢失时不返回迟到的成功结果。
func (controller *Controller) OpenBrowserWindow(ctx context.Context, address sharedbrowser.Address) (result sharedbrowser.Window, failure error) {
	failure = controller.withBrowser(ctx, address.Session, func(ctx context.Context, browser sharedbrowser.Browser) (err error) {
		result, err = browser.Open(ctx, address)
		return err
	})
	if failure != nil {
		result = sharedbrowser.Window{}
	}
	return result, failure
}

// ExecuteBrowserCommand 委托原实例核对窗口归属和动作序号，不在代理层重放网页副作用。
func (controller *Controller) ExecuteBrowserCommand(ctx context.Context, address sharedbrowser.Address, command sharedbrowser.Command) (result sharedbrowser.Window, failure error) {
	failure = controller.withBrowser(ctx, address.Session, func(ctx context.Context, browser sharedbrowser.Browser) (err error) {
		result, err = browser.Execute(ctx, address, command)
		return err
	})
	if failure != nil {
		result = sharedbrowser.Window{}
	}
	return result, failure
}

// BrowserScreenshot 返回所属页面的真实 PNG；撤销期间完成的像素不会离开代理。
func (controller *Controller) BrowserScreenshot(ctx context.Context, address sharedbrowser.Address) (result []byte, failure error) {
	failure = controller.withBrowser(ctx, address.Session, func(ctx context.Context, browser sharedbrowser.Browser) (err error) {
		result, err = browser.Screenshot(ctx, address)
		return err
	})
	if failure != nil {
		result = nil
	}
	return result, failure
}

// RespondBrowserDialog 可与正在等待对话框的网页动作并行，仍复验相同完整会话。
func (controller *Controller) RespondBrowserDialog(ctx context.Context, address sharedbrowser.Address, reply sharedbrowser.DialogReply) error {
	return controller.withBrowser(ctx, address.Session, func(ctx context.Context, browser sharedbrowser.Browser) error {
		return browser.RespondDialog(ctx, address, reply)
	})
}

// CloseBrowserWindow 只关闭当前会话在原实例中的指定页面，结果不明不在新实例补发。
func (controller *Controller) CloseBrowserWindow(ctx context.Context, address sharedbrowser.Address) error {
	return controller.withBrowser(ctx, address.Session, func(ctx context.Context, browser sharedbrowser.Browser) error {
		return browser.CloseWindow(ctx, address)
	})
}

// WindowFactory 在用户程序启动前绑定窗口入口；失败时不得遗留监听或页面，不能借用其他会话。
type WindowFactory func(processgroup.Reference) (WindowSession, error)

// windowSource 固定原 Chrome 实例，不在服务重启后把旧桌面入口重定向到新实例。
func (controller *BrowserController) windowSource(session computer.ExecutionSession) (*browserInstance, error) {
	instance := controller.available.Load()
	if session.Validate() != nil || instance == nil || !controller.identity.matches(session) || session.ContainerID != instance.grant.Request.Group.Container.ContainerID || instance.stopping.Load() || controller.lifetime.Err() != nil {
		return nil, errors.New("浏览器桌面入口没有有效的共享实例")
	}
	select {
	case <-instance.scope.Done():
		return nil, errors.New("浏览器桌面入口的共享实例已结束")
	default:
	}
	return instance, nil
}

type controllerShutdown interface {
	Shutdown(context.Context) error
}

// shutdownControllers 先回收桌面页面与探针，再关闭共享 Chrome，防止账号服务先退出造成页面回收未知。
func shutdownControllers(ctx context.Context, desktops, probes, browsers controllerShutdown) error {
	results := make(chan error, 2)
	go func() { results <- desktops.Shutdown(ctx) }()
	go func() { results <- probes.Shutdown(ctx) }()
	var failure error
	for range 2 {
		select {
		case err := <-results:
			failure = errors.Join(failure, err)
		case <-ctx.Done():
			return errors.Join(failure, ctx.Err())
		}
	}
	if failure != nil {
		return failure
	}
	return browsers.Shutdown(ctx)
}
