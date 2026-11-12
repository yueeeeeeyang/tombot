package computeragent

import (
	"context"
	"testing"
)

// deferredCancellation 模拟 Done 已关闭、取消通知尚未获得调度的窗口，不改变生产时限。
type deferredCancellation struct {
	context.Context
	done     chan struct{}
	callback func()
}

func (ctx *deferredCancellation) Done() <-chan struct{} { return ctx.done }
func (ctx *deferredCancellation) Err() error {
	select {
	case <-ctx.done:
		return context.Canceled
	default:
		return nil
	}
}
func (ctx *deferredCancellation) AfterFunc(callback func()) func() bool {
	ctx.callback = callback
	return func() bool {
		if ctx.callback == nil {
			return false
		}
		ctx.callback = nil
		return true
	}
}

func TestBrowserCancellationBeforeCallbackDelivery(t *testing.T) {
	ctx := &deferredCancellation{Context: context.Background(), done: make(chan struct{})}
	scope := &fakeBrowserScope{done: make(chan struct{})}
	instance := &browserInstance{scope: scope}
	finish := (&BrowserController{}).cancelScope(ctx, instance)
	close(ctx.done)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if ctx.callback != nil {
		t.Fatal("取消回调未汇合")
	}
	if !scope.revoked.Load() || !instance.stopping.Load() {
		t.Fatal("已取消调用停止了尚未调度的撤销通知")
	}
}

// TestBrowserCancellationCannotBeStoppedAfterCancellation 覆盖启动返回与撤销回调尚未调度的竞争。
func TestBrowserCancellationCannotBeStoppedAfterCancellation(t *testing.T) {
	for range 128 {
		ctx, cancel := context.WithCancel(t.Context())
		scope := &fakeBrowserScope{done: make(chan struct{})}
		instance := &browserInstance{scope: scope}
		finish := (&BrowserController{}).cancelScope(ctx, instance)
		cancel()
		if err := finish(); err != nil {
			t.Fatal(err)
		}
		if !scope.revoked.Load() || !instance.stopping.Load() {
			t.Fatal("已取消调用被完成路径抢先停止，未撤销浏览器")
		}
	}
}
