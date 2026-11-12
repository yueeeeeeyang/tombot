package service

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func TestBrowserScreenUserOnlyActionsAndStateFailure(t *testing.T) {
	t.Parallel()
	s, engine, _, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	x, y := 10.0, 20.0
	for _, action := range []BrowserAction{{BrowserVersion: browserVersion(tab), Action: "input", Text: "模型不得绕过引用"}, {BrowserVersion: browserVersion(tab), Action: "click_at", X: &x, Y: &y}} {
		if _, err = s.Apply(ctx, tab.ID, action, &run, "bot"); browserCode(err) != "BROWSER_USER_ACTION_REQUIRED" {
			t.Fatal("模型获得用户画面动作", err)
		}
	}
	tab, err = s.SetController(ctx, tab.ID, "user", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	frame, err := s.CaptureScreen(ctx, tab.ID, 60)
	if err != nil || frame.Tab.ID != tab.ID || frame.Tab.Controller != "user" || len(frame.JPEG) == 0 {
		t.Fatal("画面状态不一致", err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(frame.Tab), Action: "click_at", X: &x, Y: &y}, nil, "user"); err != nil {
		t.Fatal(err)
	}
	engine.SetFailure(domain.NewError(503, "BROWSER_NAVIGATION_GUARD_FAILED", "Protection failed."))
	if _, err = s.CaptureScreen(ctx, tab.ID, 60); browserCode(err) != "BROWSER_NAVIGATION_GUARD_FAILED" {
		t.Fatal("保护失败仍发送画面", err)
	}
	engine.SetFailure(nil)
	if _, err = s.db.Exec("CREATE TRIGGER reject_screen_sync BEFORE UPDATE ON browser_tabs BEGIN SELECT RAISE(ABORT,'state unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	engine.Change(tab.ID, "https://example.com/changed")
	if _, err = s.CaptureScreen(ctx, tab.ID, 60); browserCode(err) != "BROWSER_STATE_FAILED" {
		t.Fatal("画面状态无法保存仍返回画面", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_screen_sync"); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx, "user"); err != nil {
		t.Fatal("状态修复后无法恢复画面服务", err)
	}
}
