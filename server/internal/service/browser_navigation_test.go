package service

import (
	"context"
	"fmt"
	"testing"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/browserfake"
)

// delayedBrowserNotifications 模拟页面已更新，但后台同步通知尚未送达。
type delayedBrowserNotifications struct{ *browserfake.Engine }

func (delayedBrowserNotifications) Changed() <-chan struct{} { return nil }

func TestBrowserHandoffUsesCurrentRuntimeOrigin(t *testing.T) {
	ctx := context.Background()
	previous, _, bot, _ := browserServiceFixture(t)
	if err := previous.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var engine *browserfake.Engine
	s, err := NewBrowser(ctx, BrowserDependencies{DB: previous.db, Bots: previous.bots, Events: previous.events, Logger: previous.logger, Factory: func(_ context.Context, options browser.Options) (BrowserEngine, error) {
		engine = browserfake.New(options)
		return delayedBrowserNotifications{engine}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	tab, err := s.Open(ctx, BrowserOpenInput{BotID: bot.ID, URL: "https://old.example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	engine.Change(tab.ID, "https://current.example.com")
	stored, err := s.Get(ctx, tab.ID)
	if err != nil || stored.URL != "https://old.example.com" {
		t.Fatal("未构造通知延迟", err)
	}
	if _, err = s.SetController(ctx, tab.ID, "bot", tab.Revision, "user"); err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{"https://old.example.com": false, "https://current.example.com": true} {
		if allowed, err := s.CanVisit(ctx, bot.ID, tab.ID, address); err != nil || allowed != want {
			t.Fatal("交接批准了过时来源", address, err)
		}
	}
}

func TestBrowserNavigationPermissionsAndExplicitDeny(t *testing.T) {
	t.Parallel()
	s, engine, bot, run := browserServiceFixture(t)
	ctx := context.Background()
	allowed, err := s.CanVisit(ctx, bot.ID, "", "https://example.com")
	if err != nil || allowed {
		t.Fatal("新源默认获准", err)
	}
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com/path"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	request := browser.Navigation{TabID: tab.ID, Owner: bot.ID, URL: "https://EXAMPLE.com:443/next", Method: "GET"}
	if err = s.authorizeNavigation(ctx, request); err != nil {
		t.Fatal("已批准的同源导航被拒绝", err)
	}
	for _, address := range []string{"http://example.com/", "https://sub.example.com/", "https://example.com:8443/"} {
		request.URL = address
		if err = s.authorizeNavigation(ctx, request); browserCode(err) != "BROWSER_DOMAIN_APPROVAL_REQUIRED" {
			t.Fatal("许可扩展到其他源", err)
		}
	}
	popup := browser.Tab{ID: ids.New(), BotID: bot.ID, URL: "https://example.com/popup", Revision: 1}
	if err = engine.Popup(ctx, tab.ID, popup, nil); err != nil {
		t.Fatal(err)
	}
	if allowed, err = s.CanVisit(ctx, bot.ID, popup.ID, "https://example.com"); err != nil || !allowed {
		t.Fatal("弹窗未继承已批准源", err)
	}
	origin := "https://example.com"
	approvals := NewApprovals(s.db, s.events)
	if _, err = approvals.SaveRule(ctx, PolicyRule{BotID: bot.ID, ActionClass: "net.new_domain", Pattern: &origin, Decision: "deny"}, "user"); err != nil {
		t.Fatal(err)
	}
	request.URL = origin
	if err = s.authorizeNavigation(ctx, request); browserCode(err) != "TOOL_DENIED" {
		t.Fatal("显式拒绝未覆盖既有许可", err)
	}
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM browser_origin_permissions WHERE tab_id=?", tab.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("关闭标签未清理许可", err)
	}
}

func TestBrowserOriginLimitPreventsEffectsAndKeepsExistingPermission(t *testing.T) {
	t.Parallel()
	s, _, bot, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < browserOriginLimit; i++ {
		if _, err = s.db.Exec("INSERT INTO browser_origin_permissions(tab_id,origin,created_at) VALUES(?,?,0)", tab.ID, fmt.Sprintf("https://site-%d.example.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(tab), Action: "navigate", URL: "https://new.example.com"}, &run, "bot")
	if browserCode(err) != "BROWSER_ORIGIN_LIMIT" {
		t.Fatal("来源许可超量仍执行导航", err)
	}
	var actions int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='browser.action.start'").Scan(&actions); err != nil || actions != 0 {
		t.Fatal("超量来源产生了页面副作用", err)
	}
	if allowed, err := s.CanVisit(ctx, bot.ID, tab.ID, "https://new.example.com"); err != nil || allowed {
		t.Fatal("失败事务留下来源许可", err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(tab), Action: "navigate", URL: "https://example.com/next"}, &run, "bot"); err != nil {
		t.Fatal("达到容量后既有许可无法使用", err)
	}
}

func TestBrowserNavigationUserTakeoverAndGrantAuditFailure(t *testing.T) {
	t.Parallel()
	s, _, bot, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{BotID: bot.ID, URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	request := browser.Navigation{TabID: tab.ID, Owner: bot.ID, URL: "https://other.example.com", Method: "POST"}
	if err = s.authorizeNavigation(ctx, request); err != nil {
		t.Fatal("用户接管时仍按 Bot 许可拒绝", err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER reject_browser_grant BEFORE INSERT ON audit_log WHEN NEW.action='browser.origin.granted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetController(ctx, tab.ID, "bot", tab.Revision, "user"); err == nil {
		t.Fatal("许可审计失败仍交接")
	}
	tab, err = s.Get(ctx, tab.ID)
	if err != nil || tab.Controller != "user" {
		t.Fatal("失败事务未回滚", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_browser_grant"); err != nil {
		t.Fatal(err)
	}
	tab, err = s.SetController(ctx, tab.ID, "bot", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.authorizeNavigation(ctx, request); browserCode(err) != "BROWSER_DOMAIN_APPROVAL_REQUIRED" {
		t.Fatal("交接继承了未批准源", err)
	}
	request.URL = "https://example.com"
	if err = s.authorizeNavigation(ctx, request); err != nil {
		t.Fatal("交接未批准当前页面源", err)
	}
}
