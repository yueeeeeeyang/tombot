//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
)

func TestContainerWindowDirectBrowserCalls(t *testing.T) {
	f := newWindowFixture(t)
	windows := f.open(t, 0, testWindowPeer)
	f.guard.scope = &borrowedBrowserScope{Scope: f.guard.scope, windows: windows}
	address := sharedbrowser.Address{Session: windows.session, WindowID: ids.New()}
	if page, err := f.controller.OpenBrowserWindow(t.Context(), address); err != nil || page.ID != address.WindowID {
		t.Fatal("直接入口无法操作原浏览器", err)
	}
	if pages, err := f.controller.BrowserWindows(t.Context(), windows.session); err != nil || len(pages) != 1 || pages[0].ID != address.WindowID {
		t.Fatal("直接入口未返回所属页面", err)
	}
	wrong := windows.session
	wrong.SessionID = ids.New()
	if err := windows.UseBrowser(t.Context(), wrong, func(context.Context, sharedbrowser.Browser) error {
		t.Error("错误会话进入了浏览器")
		return nil
	}); !errors.Is(err, sharedbrowser.ErrDenied) {
		t.Fatal("错误会话未拒绝", err)
	}
	if err := windows.UseBrowser(t.Context(), windows.session, nil); !errors.Is(err, sharedbrowser.ErrDenied) {
		t.Fatal("缺少动作未拒绝", err)
	}
	windows.Revoke()
	if _, err := f.controller.BrowserWindows(t.Context(), windows.session); err == nil {
		t.Fatal("已撤销入口仍接受直接调用")
	}
}

func TestContainerWindowDirectCancellationAndRetirementBarrier(t *testing.T) {
	f := newWindowFixture(t)
	windows := f.open(t, 0, testWindowPeer)
	f.guard.scope = &borrowedBrowserScope{Scope: f.guard.scope, windows: windows}
	entered, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	defer release()
	var active atomic.Bool
	f.records.list = func(ctx context.Context) error {
		active.Store(true)
		defer active.Store(false)
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-finish
		return nil // 模拟网页已经返回，但会话在回执抵达前被撤销。
	}
	f.records.release = func(context.Context) error {
		if active.Load() {
			t.Error("直接调用退出前开始页面回收")
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		result, err := f.controller.BrowserWindows(t.Context(), windows.session)
		if result != nil {
			t.Error("撤销后的迟到页面列表被返回")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("直接调用未进入")
	}
	bounded, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := f.controller.withScope(bounded, windows.session, func(context.Context, *desktopGuard) error { return nil }); err != nil {
		t.Fatal("网页调用占用桌面授权锁", err)
	}
	cancel()
	windows.Revoke()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("撤销未取消直接调用")
	}
	select {
	case <-windows.retired:
		t.Error("直接调用未退出就报告页面回收完成")
	default:
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, sharedbrowser.ErrUnknown) {
			t.Fatal("迟到成功未转为未知", err)
		}
	case <-time.After(time.Second):
		t.Fatal("直接调用未退出")
	}
	if err := windows.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.fatals.Load() != 0 {
		t.Fatal("正常取消被当作回收故障")
	}
}

func TestContainerWindowDirectCallRechecksOriginalInstance(t *testing.T) {
	f := newWindowFixture(t)
	windows := f.open(t, 0, testWindowPeer)
	defer f.browsers.available.Store(windows.instance)
	for _, before := range []bool{true, false} {
		f.browsers.available.Store(windows.instance)
		if before {
			f.browsers.available.Store(nil)
		}
		called := false
		err := windows.UseBrowser(t.Context(), windows.session, func(context.Context, sharedbrowser.Browser) error {
			called = true
			f.browsers.available.Store(nil)
			return nil
		})
		if err == nil || called == before {
			t.Fatal("未在直接调用前后复验原实例", before, called, err)
		}
		if before && !errors.Is(err, sharedbrowser.ErrDenied) {
			t.Fatal("调用前失去原实例未拒绝", err)
		}
		if !before && !errors.Is(err, sharedbrowser.ErrUnknown) {
			t.Fatal("网页动作后的实例变更未报告未知", err)
		}
	}
	f.browsers.available.Store(windows.instance)
	// 相同 Bot 的旧执行权不能通过当前 root 入口借用新会话。
	other := windows.session
	other.FencingToken++
	if err := windows.UseBrowser(t.Context(), other, func(context.Context, sharedbrowser.Browser) error { return nil }); err == nil {
		t.Fatal("旧控制代次未拒绝")
	}
}
