//go:build linux

package computeragent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/sharedbrowser"
)

type retiringBrowser struct {
	unusedWindowBrowser
	releases atomic.Int32
}

func (browser *retiringBrowser) ReleaseSession(context.Context, computer.ExecutionSession) error {
	browser.releases.Add(1)
	return nil
}

// 不依赖容器或关闭竞态检测即可验证：调用忽略取消时，回收必须有界失败，不能提前释放页面。
func TestDirectBrowserRetirementRejectsUnresponsiveCall(t *testing.T) {
	f := newFixture(t)
	life, stop := context.WithCancel(t.Context())
	defer stop()
	service := &retiringBrowser{}
	var fatals atomic.Int32
	windows := &windowSession{
		session: f.command.Session, instance: &browserInstance{service: service},
		life: life, stop: stop, logger: f.guard.logger,
		options:   Options{CallTimeout: time.Second, CleanupTimeout: 100 * time.Millisecond},
		authorize: func(ctx context.Context, _ computer.ExecutionSession) error { return ctx.Err() },
		onFatal:   func() { fatals.Add(1) }, done: make(chan struct{}), retired: make(chan struct{}),
	}
	close(windows.done) // 网关已经退出，仍须等待直接调用退出。
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	defer release()
	returned := make(chan error, 1)
	go func() {
		returned <- windows.UseBrowser(t.Context(), windows.session, func(context.Context, sharedbrowser.Browser) error {
			close(entered)
			<-finish
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("测试调用未进入")
	}
	windows.Revoke()
	select {
	case <-windows.retired:
	case <-time.After(time.Second):
		t.Fatal("阻塞调用导致回收看门狗失效")
	}
	if !errors.Is(windows.retireError, context.DeadlineExceeded) || fatals.Load() != 1 || service.releases.Load() != 0 {
		t.Fatal("阻塞调用被错误报告为已回收", windows.retireError, fatals.Load(), service.releases.Load())
	}
	release()
	select {
	case err := <-returned:
		if !errors.Is(err, sharedbrowser.ErrUnknown) {
			t.Fatal("阻塞调用的迟到成功未丢弃", err)
		}
	case <-time.After(time.Second):
		t.Fatal("测试调用未退出")
	}
}

func TestDirectBrowserCancellationDuringFinalAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	windows := &windowSession{
		session: f.command.Session, instance: &browserInstance{service: unusedWindowBrowser{}},
		life: t.Context(), options: Options{CallTimeout: time.Second},
		authorize: func(context.Context, computer.ExecutionSession) error {
			checks++
			if checks == 2 {
				cancel()
			}
			return nil
		},
	}
	err := windows.UseBrowser(ctx, windows.session, func(context.Context, sharedbrowser.Browser) error { return sharedbrowser.ErrDenied })
	if checks != 2 || !errors.Is(err, sharedbrowser.ErrUnknown) || errors.Is(err, sharedbrowser.ErrDenied) {
		t.Fatal("授权复验期间取消仍返回动作成功", checks, err)
	}
}
