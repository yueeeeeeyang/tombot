package sharedbrowser

import (
	"context"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// prepareNativeNavigation 通过桌面键盘导航，协议只观察新文档是否加载，不能把按下回车当成页面完成。
func (service *Service) prepareNativeNavigation(ctx context.Context, item *entry, command Command) (func(context.Context) error, error) {
	target := chromedp.FromContext(item.ctx).Target
	before, err := cdp.Call(ctx, target, page.GetFrameTree, cdp.Empty{})
	if err != nil || before.FrameTree == nil || before.FrameTree.Frame == nil {
		return nil, ErrUnavailable
	}
	if command.Kind == "back" || command.Kind == "forward" {
		history, err := cdp.Call(ctx, target, page.GetNavigationHistory, cdp.Empty{})
		if err != nil {
			return nil, ErrUnavailable
		}
		if command.Kind == "back" && history.CurrentIndex == 0 || command.Kind == "forward" && int(history.CurrentIndex) == len(history.Entries)-1 {
			return nil, ErrNavigation
		}
	}
	return func(ctx context.Context) error {
		if err := service.command(ctx, item, command); err != nil {
			return err
		}
		timer := time.NewTicker(30 * time.Millisecond)
		defer timer.Stop()
		for {
			current, err := cdp.Call(ctx, target, page.GetFrameTree, cdp.Empty{})
			if err != nil {
				return err
			}
			if current.FrameTree != nil && current.FrameTree.Frame != nil {
				frame := current.FrameTree.Frame
				if frame.LoaderID != before.FrameTree.Frame.LoaderID || frame.URL != before.FrameTree.Frame.URL {
					ready, err := cdp.Call(ctx, target, runtime.Evaluate, runtime.EvaluateParams{Expression: "document.readyState"})
					if err != nil {
						return err
					}
					if ready.ExceptionDetails == nil && ready.Result != nil && string(ready.Result.Value) == `"complete"` {
						return nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}, nil
}
