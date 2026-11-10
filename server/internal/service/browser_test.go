package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/testkit/browserfake"
)

func browserServiceFixture(t *testing.T) (*Browser, *browserfake.Engine, domain.Bot, domain.Run) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	bots := NewBots(db, events, t.TempDir())
	bot, err := bots.Create(context.Background(), BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	var engine *browserfake.Engine
	s, err := NewBrowser(context.Background(), BrowserDependencies{DB: db, Bots: bots, Events: events, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Factory: func(ctx context.Context, options browser.Options) (BrowserEngine, error) {
		engine = browserfake.New(options)
		return engine, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), ConversationID: bot.ConversationID, BotID: bot.ID}
	if _, err = db.Exec("INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)", run.TurnID, run.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,'chat','running','fixture','Use the browser.',0)", run.ID, run.TurnID, run.ConversationID, run.BotID); err != nil {
		t.Fatal(err)
	}
	return s, engine, bot, run
}
func browserVersion(tab BrowserTab) BrowserVersion {
	return BrowserVersion{Revision: tab.Revision, PageRevision: tab.PageRevision}
}
func browserCode(err error) string {
	var public *domain.Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}

func TestBrowserServiceOwnershipVersionsAndAudit(t *testing.T) {
	t.Parallel()
	s, engine, bot, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if tab.BotID != bot.ID || tab.Controller != "bot" || tab.Status != "active" {
		t.Fatal("标签归属未保存")
	}
	old := browserVersion(tab)
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: old, Action: "key", Key: "Enter"}, nil, "user"); browserCode(err) != "BROWSER_CONTROL_REQUIRED" {
		t.Fatal("未接管可输入", err)
	}
	other := run
	other.BotID = "other"
	if _, err = s.Snapshot(ctx, tab.ID, &other); browserCode(err) != "BROWSER_TAB_FORBIDDEN" {
		t.Fatal("越权读取", err)
	}
	tab, err = s.SetController(ctx, tab.ID, "user", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	tab, err = s.SetController(ctx, tab.ID, "bot", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: old, Action: "click", Ref: "e1"}, &run, "bot"); browserCode(err) != "BROWSER_TAB_CHANGED" {
		t.Fatal("旧审批未失效", err)
	}
	snapshot, err := s.Snapshot(ctx, tab.ID, &run)
	if err != nil {
		t.Fatal(err)
	}
	tab, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "type", Ref: "e1", Text: "测试输入正文"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if tab.PageRevision <= snapshot.Tab.PageRevision {
		t.Fatal("页面版本没有递增")
	}
	var details string
	if err = s.db.QueryRow("SELECT COALESCE(GROUP_CONCAT(payload_json),'') FROM events").Scan(&details); err != nil || strings.Contains(details, "测试输入正文") {
		t.Fatal("事件泄漏了输入正文", err)
	}
	old = browserVersion(tab)
	engine.Change(tab.ID, "https://example.com/next")
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: old, Action: "key", Key: "Enter"}, &run, "bot"); browserCode(err) != "BROWSER_TAB_CHANGED" {
		t.Fatal("跳转后仍执行旧动作", err)
	}
	if len(engine.Operations()) != 2 {
		t.Fatal("拒绝的动作触发了浏览器", engine.Operations())
	}
}

// awaitBrowserAvailability 在实际总线读取故障或恢复通知，同时核对状态读取使用相同口径。
func awaitBrowserAvailability(t *testing.T, s *Browser, cursor int64, available bool) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	status, err := s.Status(ctx)
	if err != nil || status.Available != available {
		t.Fatal("能力状态错误", status, err)
	}
	for {
		wake := s.events.Wake()
		events, gap, err := s.events.Read(ctx, cursor)
		if err != nil || gap {
			t.Fatal("读取能力变更失败", err)
		}
		for _, event := range events {
			cursor = event.ID
			if event.Kind != "browser.availability" {
				continue
			}
			var observed browserAvailability
			if err = json.Unmarshal(event.Payload, &observed); err != nil {
				t.Fatal(err)
			}
			if observed.Available == available && observed.State == status.State && observed.StateError == status.StateError {
				return cursor
			}
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("没有收到能力变更通知", ctx.Err())
		}
	}
}

func TestBrowserServiceFailuresStopEffectsAndRecoverWithoutReplay(t *testing.T) {
	t.Parallel()
	s, engine, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	input := BrowserAction{BrowserVersion: browserVersion(tab), Action: "key", Key: "Enter"}
	tab, err = s.Apply(ctx, tab.ID, input, nil, "user")
	if err != nil || tab.Status != "active" {
		t.Fatal("按键未立即执行", err)
	}
	if len(engine.Operations()) != 2 {
		t.Fatal("打开和按键次数不符", engine.Operations())
	}
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err != nil {
		t.Fatal("重复关闭不幂等", err)
	}
}

func TestBrowserServiceCancellationPersistsAndInactiveRunCannotAct(t *testing.T) {
	t.Parallel()
	s, engine, _, run := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	engine.SetActionHook(func(ctx context.Context) error { cancel(); return ctx.Err() })
	_, err = s.Apply(canceled, tab.ID, BrowserAction{BrowserVersion: browserVersion(tab), Action: "click", Ref: "e1"}, &run, "bot")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	tab, err = s.Get(ctx, tab.ID)
	if err != nil || tab.Status != "active" || tab.ErrorCode != "BROWSER_CANCELED" {
		t.Fatal("取消后未保存结果", err)
	}
	engine.SetActionHook(nil)
	if _, err = s.db.Exec("UPDATE runs SET status='canceled',finished_at=0 WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(tab), Action: "key", Key: "Enter"}, &run, "bot"); browserCode(err) != "RUN_NOT_ACTIVE" {
		t.Fatal("已取消运行可继续动作", err)
	}
	if len(engine.Operations()) != 2 {
		t.Fatal("取消后产生额外动作")
	}
}

func TestBrowserPopupSynchronizationIsAtomic(t *testing.T) {
	t.Parallel()
	s, engine, _, run := browserServiceFixture(t)
	ctx := context.Background()
	parent, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	popup := browser.Tab{ID: ids.New(), BotID: run.BotID, URL: "https://example.com/popup", Revision: 1}
	persisted, adopt := make(chan struct{}), make(chan struct{})
	popupDone := make(chan error, 1)
	go func() { popupDone <- engine.Popup(ctx, parent.ID, popup, func() { close(persisted); <-adopt }) }()
	<-persisted
	synced := make(chan error, 1)
	go func() {
		if err := s.acquire(ctx); err != nil {
			synced <- err
			return
		}
		defer s.release()
		synced <- s.sync(ctx)
	}()
	select {
	case err := <-synced:
		close(adopt)
		t.Fatal("同步越过尚未完成的弹窗接管", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(adopt)
	if err := <-popupDone; err != nil {
		t.Fatal(err)
	}
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(ctx, popup.ID)
	if err != nil || stored.BotID != run.BotID || stored.Controller != "bot" {
		t.Fatal("弹窗记录丢失或未继承控制权", err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER reject_popup BEFORE INSERT ON browser_tabs BEGIN SELECT RAISE(ABORT,'state unavailable'); END"); err != nil {
		t.Fatal(err)
	}
	popup.ID = ids.New()
	if err = engine.Popup(ctx, parent.ID, popup, nil); err == nil {
		t.Fatal("落库失败仍接管弹窗")
	}
	tabs, err := engine.List(ctx, "")
	if err != nil || len(tabs) != 2 {
		t.Fatal("拒绝弹窗仍残留运行时标签", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_popup"); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserServiceCapacityFailedOpenAndDeletionGuard(t *testing.T) {
	t.Parallel()
	s, engine, bot, run := browserServiceFixture(t)
	ctx := context.Background()
	if _, err := s.Open(ctx, BrowserOpenInput{URL: "file:///tmp/report"}, nil, "user"); err == nil {
		t.Fatal("非法 URL 未拒绝")
	}
	if _, err := s.Open(ctx, BrowserOpenInput{BotID: "other", URL: "https://example.com"}, &run, "bot"); err == nil {
		t.Fatal("跨 Bot 打开未拒绝")
	}
	engine.SetFailure(domain.NewError(503, "BROWSER_UNAVAILABLE", "Unavailable."))
	for i := 0; i < browser.MaxOwnerTabs; i++ {
		tab, err := s.Open(ctx, BrowserOpenInput{BotID: bot.ID, URL: "https://example.com"}, nil, "user")
		if browserCode(err) != "BROWSER_UNAVAILABLE" || tab.Status != "failed" {
			t.Fatal("失败记录丢失", err)
		}
	}
	if _, err := s.Open(ctx, BrowserOpenInput{BotID: bot.ID, URL: "https://example.com"}, nil, "user"); browserCode(err) != "BROWSER_TAB_LIMIT" {
		t.Fatal("标签数量无界", err)
	}
	if _, err := s.db.Exec("UPDATE runs SET status='succeeded',finished_at=0 WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.bots.Delete(ctx, bot.ID, bot.Name, "user", false); browserCode(err) != "BOT_BROWSER_TABS_OPEN" {
		t.Fatal("删除未关闭标签的 Bot", err)
	}
	engine.SetFailure(nil)
	tabs, err := s.List(ctx, bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tab := range tabs {
		if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.bots.Delete(ctx, bot.ID, bot.Name, "user", false); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserServiceBotCanCloseOwnFailedTab(t *testing.T) {
	t.Parallel()
	s, engine, _, run := browserServiceFixture(t)
	ctx := context.Background()
	engine.SetFailure(domain.NewError(503, "BROWSER_UNAVAILABLE", "Unavailable."))
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if browserCode(err) != "BROWSER_UNAVAILABLE" || tab.Status != "failed" {
		t.Fatal("打开失败未保存记录", err)
	}
	engine.SetFailure(nil)
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), &run, "bot"); err != nil {
		t.Fatal("Bot 无法清理自己的失败标签", err)
	}
	tabs, err := s.List(ctx, run.BotID)
	if err != nil || len(tabs) != 0 {
		t.Fatal("失败标签未释放容量", err)
	}
}

func TestBrowserServiceRestartRestoresOnlyConfirmedTabs(t *testing.T) {
	t.Parallel()
	s, _, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	confirmed, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com/form"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	closing, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com/closing"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE browser_tabs SET status='acting' WHERE id=?", interrupted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE browser_tabs SET status='closing' WHERE id=?", closing.ID); err != nil {
		t.Fatal(err)
	}
	var engine *browserfake.Engine
	restarted, err := NewBrowser(ctx, BrowserDependencies{DB: s.db, Bots: s.bots, Events: s.events, Logger: s.logger, Factory: func(ctx context.Context, options browser.Options) (BrowserEngine, error) {
		engine = browserfake.New(options)
		return engine, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restarted.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	tabs, err := engine.List(ctx, "")
	if err != nil || len(tabs) != 1 || tabs[0].ID != confirmed.ID || tabs[0].Revision != confirmed.PageRevision {
		t.Fatal("恢复了未确认动作或丢失身份", err)
	}
	interrupted, err = restarted.Get(ctx, interrupted.ID)
	if err != nil || interrupted.Status != "failed" {
		t.Fatal("中断动作未标记", err)
	}
	if _, err = restarted.Get(ctx, closing.ID); !isBrowserMissing(err) {
		t.Fatal("关闭意图被重开", err)
	}
	if len(engine.Operations()) != 0 {
		t.Fatal("构造服务时自动重放动作")
	}
}

func TestBrowserServiceUnsupportedNavigationRequiresExplicitRestore(t *testing.T) {
	t.Parallel()
	s, engine, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	engine.Change(tab.ID, "chrome-error://chromewebdata/")
	if err = s.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	err = s.sync(ctx)
	s.release()
	if err != nil {
		t.Fatal(err)
	}
	tab, err = s.Get(ctx, tab.ID)
	if err != nil || tab.URL != "https://example.com" || tab.ErrorCode != "BROWSER_URL_UNSUPPORTED" {
		t.Fatal("不支持的页面污染恢复地址或未标记异常", err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(tab), Action: "key", Key: "Enter"}, nil, "user"); browserCode(err) != "BROWSER_TAB_INACTIVE" {
		t.Fatal("未知页面仍可输入", err)
	}
	tab, err = s.RestoreTab(ctx, tab.ID, tab.Revision, "user")
	if err != nil || tab.Status != "active" || tab.ErrorCode != "" {
		t.Fatal("无法显式恢复已验证地址", err)
	}
}

func TestBrowserServiceOpenCompletionAndCloseFailuresStayVisible(t *testing.T) {
	t.Parallel()
	s, engine, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil || tab.Status != "active" {
		t.Fatal("打开未完成", err)
	}
	if len(engine.Operations()) != 1 {
		t.Fatal("打开产生了额外操作")
	}
	engine.SetFailure(domain.NewError(502, "BROWSER_OPERATION_FAILED", "The close failed."))
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err == nil {
		t.Fatal("关闭失败被吞掉")
	}
	tab, err = s.Get(ctx, tab.ID)
	if err != nil || tab.ErrorCode != "BROWSER_OPERATION_FAILED" {
		t.Fatal("关闭失败丢失标签记录", err)
	}
	engine.SetFailure(nil)
	if err = s.CloseTab(ctx, tab.ID, browserVersion(tab), nil, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserServiceDisappearingTabDoesNotReportSuccess(t *testing.T) {
	t.Parallel()
	s, engine, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if _, err = s.db.Exec("UPDATE browser_tabs SET status='acting' WHERE id=?", tab.ID); err != nil {
		t.Fatal(err)
	}
	// 模拟页面动作关闭自身，协议成功返回但目标已在完成结果保存前消失。
	if err = engine.CloseTab(ctx, tab.selection()); err != nil {
		t.Fatal(err)
	}
	tab, err = s.finish(tab, nil, "user", "action")
	if browserCode(err) != "BROWSER_TAB_LOST" || tab.Status != "failed" || tab.ErrorCode != "BROWSER_TAB_LOST" {
		t.Fatal("消失的目标被报告为成功", err)
	}
}

func TestBrowserServiceLateValidAddressClearsTransientFailure(t *testing.T) {
	t.Parallel()
	s, _, _, _ := browserServiceFixture(t)
	ctx := context.Background()
	tab, err := s.Open(ctx, BrowserOpenInput{URL: "https://example.com/done"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.release()
	// 导航版本已保存，地址仍为过渡值；后续有效地址到达时不一定再触发版本变化。
	if _, err = s.db.Exec("UPDATE browser_tabs SET error_code='BROWSER_URL_UNSUPPORTED' WHERE id=?", tab.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.sync(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, tab.ID)
	if err != nil || current.ErrorCode != "" || current.PageRevision != tab.PageRevision {
		t.Fatal("有效地址未解除过渡期错误", err, current.ErrorCode)
	}
	if _, err = s.selected(ctx, tab.ID, nil, browserVersion(current), true, false); err != nil {
		t.Fatal("地址恢复后仍无法继续操作", err)
	}
}
