package computeragent

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
)

type borrowedBrowserScope struct {
	Scope
	windows WindowSession
}

func (scope *borrowedBrowserScope) BrowserSession(computer.ExecutionSession) (WindowSession, error) {
	return scope.windows, nil
}

type borrowedBrowserSession struct {
	use func(context.Context, computer.ExecutionSession, func(context.Context, sharedbrowser.Browser) error) error
}

func (session *borrowedBrowserSession) UseBrowser(ctx context.Context, owner computer.ExecutionSession, action func(context.Context, sharedbrowser.Browser) error) error {
	return session.use(ctx, owner, action)
}
func (*borrowedBrowserSession) Revoke()                     {}
func (*borrowedBrowserSession) Close(context.Context) error { return nil }

// 根控制入口必须在网页调用前释放桌面锁，否则 Chrome 内部的授权复验会等待自己。
func TestBrowserCallReleasesDesktopLockAndRejectsWrongOwner(t *testing.T) {
	f := newFixture(t)
	if _, err := f.controller.Open(t.Context(), f.command, f.reference, f.files(t)); err != nil {
		t.Fatal(err)
	}
	called := 0
	windows := &borrowedBrowserSession{use: func(ctx context.Context, session computer.ExecutionSession, action func(context.Context, sharedbrowser.Browser) error) error {
		called++
		if session != f.command.Session {
			t.Fatal("借出了其他会话的浏览器")
		}
		bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := f.controller.withScope(bounded, session, func(context.Context, *desktopGuard) error { return nil }); err != nil {
			return errors.New("调用网页时仍占有桌面锁")
		}
		return action(ctx, unusedWindowBrowser{})
	}}
	f.guard.scope = &borrowedBrowserScope{Scope: f.guard.scope, windows: windows}
	if _, err := f.controller.BrowserWindows(t.Context(), f.command.Session); !errors.Is(err, sharedbrowser.ErrUnavailable) || called != 1 {
		t.Fatal("未将原浏览器错误返回调用方", err, called)
	}
	for _, mutate := range []func(*computer.ExecutionSession){
		func(s *computer.ExecutionSession) { s.UserID = ids.New() },
		func(s *computer.ExecutionSession) { s.BotID = ids.New() },
		func(s *computer.ExecutionSession) { s.SessionID = ids.New() },
		func(s *computer.ExecutionSession) { s.FencingToken++ },
	} {
		wrong := f.command.Session
		mutate(&wrong)
		if _, err := f.controller.BrowserWindows(t.Context(), wrong); err == nil {
			t.Fatal("错误执行身份获得浏览器")
		}
	}
	if called != 1 {
		t.Fatal("授权失败仍借用浏览器")
	}
}

type successfulBrowser struct {
	unusedWindowBrowser
	owner computer.ExecutionSession
}

func (browser successfulBrowser) List(context.Context, computer.ExecutionSession) ([]sharedbrowser.Window, error) {
	return []sharedbrowser.Window{{ID: ids.New(), BotID: browser.owner.BotID, DesktopID: browser.owner.DesktopID}}, nil
}
func (browser successfulBrowser) Open(_ context.Context, address sharedbrowser.Address) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{ID: address.WindowID}, nil
}
func (browser successfulBrowser) Execute(_ context.Context, address sharedbrowser.Address, command sharedbrowser.Command) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{ID: address.WindowID, Sequence: command.Sequence}, nil
}
func (browser successfulBrowser) Screenshot(context.Context, sharedbrowser.Address) ([]byte, error) {
	return []byte("discarded-observation"), nil
}

func TestBrowserCallDiscardsLateSuccess(t *testing.T) {
	f := newFixture(t)
	if _, err := f.controller.Open(t.Context(), f.command, f.reference, f.files(t)); err != nil {
		t.Fatal(err)
	}
	windows := &borrowedBrowserSession{use: func(ctx context.Context, session computer.ExecutionSession, action func(context.Context, sharedbrowser.Browser) error) error {
		if err := action(ctx, successfulBrowser{owner: session}); err != nil {
			return err
		}
		return sharedbrowser.ErrUnknown
	}}
	f.guard.scope = &borrowedBrowserScope{Scope: f.guard.scope, windows: windows}
	address := sharedbrowser.Address{Session: f.command.Session, WindowID: ids.New()}
	if result, err := f.controller.BrowserWindows(t.Context(), address.Session); !errors.Is(err, sharedbrowser.ErrUnknown) || result != nil {
		t.Fatal("迟到列表被返回", err)
	}
	if result, err := f.controller.OpenBrowserWindow(t.Context(), address); !errors.Is(err, sharedbrowser.ErrUnknown) || result != (sharedbrowser.Window{}) {
		t.Fatal("迟到页面被返回", err)
	}
	if result, err := f.controller.ExecuteBrowserCommand(t.Context(), address, sharedbrowser.Command{Sequence: 1, Kind: "reload"}); !errors.Is(err, sharedbrowser.ErrUnknown) || result != (sharedbrowser.Window{}) {
		t.Fatal("迟到动作被返回", err)
	}
	if result, err := f.controller.BrowserScreenshot(t.Context(), address); !errors.Is(err, sharedbrowser.ErrUnknown) || result != nil {
		t.Fatal("迟到像素被返回", err)
	}
}
