package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/testkit"
)

func newTab(owner, address string) Tab {
	return Tab{ID: ids.New(), BotID: owner, URL: address, Revision: 1}
}

func errorCode(err error) string {
	var public *domain.Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBrowserValidationAndCanceledQueue(t *testing.T) {
	for _, address := range []string{"", "file:///tmp/report", "javascript:alert(1)", "data:text/html,test", "chrome://settings", "https://user@example.com", "https://example.com\\@other.test", " https://example.com", "https://example.com\n", "https://", strings.Repeat("x", 8193)} {
		if _, err := NormalizeURL(address); errorCode(err) != "INVALID_BROWSER_INPUT" {
			t.Fatalf("非法地址未拒绝：%q", address)
		}
	}
	if address, err := NormalizeURL("https://EXAMPLE.com/path?q=中文#section"); err != nil || !strings.HasPrefix(address, "https://example.com/") {
		t.Fatal("合法地址未规范化", err)
	}
	m, err := New(context.Background(), Options{Executable: "/missing/browser", ProfileDir: filepath.Join(t.TempDir(), "profile"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err = m.Open(context.Background(), newTab("bot", "file:///tmp/report")); errorCode(err) != "INVALID_BROWSER_INPUT" {
		t.Fatal(err)
	}
	if m.started {
		t.Fatal("无效输入启动了浏览器")
	}
	if err = m.Type(context.Background(), Selection{Owner: "bot", ID: "id"}, "ref", strings.Repeat("x", MaxInputBytes+1), true); errorCode(err) != "INVALID_BROWSER_INPUT" {
		t.Fatal(err)
	}
	if err = m.Scroll(context.Background(), Selection{Owner: "bot", ID: "id"}, math.NaN(), 0); errorCode(err) != "INVALID_BROWSER_INPUT" {
		t.Fatal(err)
	}
	if err = m.Key(context.Background(), Selection{Owner: "bot", ID: "id"}, "Arbitrary"); errorCode(err) != "INVALID_BROWSER_INPUT" {
		t.Fatal(err)
	}
	if _, err = m.Screenshot(context.Background(), Selection{Owner: "bot", ID: "id"}, 101); errorCode(err) != "INVALID_BROWSER_INPUT" {
		t.Fatal(err)
	}
	if err = m.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = m.List(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("排队取消未返回", err)
	}
	m.release()
	for i := 0; i < MaxOwnerTabs; i++ {
		id := strings.Repeat("x", i+1)
		m.tabs[id] = &entry{Tab: Tab{ID: id, BotID: "bot"}}
	}
	if err = m.capacity("bot"); errorCode(err) != "BROWSER_TAB_LIMIT" {
		t.Fatal("归属上限未生效", err)
	}
	if err = m.capacity("other"); err != nil {
		t.Fatal(err)
	}
	if err = m.CloseTab(context.Background(), Selection{Owner: "other", ID: "x"}); errorCode(err) != "BROWSER_TAB_FORBIDDEN" {
		t.Fatal("跨归属关闭未拒绝", err)
	}
	if err = m.CloseTab(context.Background(), Selection{Owner: "bot", ID: "x"}); err != nil {
		t.Fatal("失效标签无法清理", err)
	}
}

func TestProfileRequiresPrivateRealDirectory(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Options{Executable: "/missing/browser", ProfileDir: public, Logger: testLogger()}); err == nil {
		t.Fatal("公开 profile 被接受")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), Options{Executable: "/missing/browser", ProfileDir: link, Logger: testLogger()}); err == nil {
		t.Fatal("符号链接 profile 被接受")
	}
}

func TestRestoreValidationIdentityAndInspectionLock(t *testing.T) {
	m, err := New(context.Background(), Options{Executable: "/missing/browser", ProfileDir: filepath.Join(t.TempDir(), "profile"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	tab := newTab("bot", "https://example.com")
	tab.Revision = 11
	for _, invalidTabs := range [][]Tab{{{ID: "bad", BotID: "bot", URL: tab.URL, Revision: 1}}, {tab, tab}, {{ID: tab.ID, BotID: "bot", URL: "file:///tmp/report", Revision: 1}}, {{ID: tab.ID, BotID: "bot", URL: tab.URL}}} {
		if err = m.Restore(context.Background(), invalidTabs); err == nil {
			t.Fatal("非法持久标签被接受")
		}
		if len(m.tabs) != 0 {
			t.Fatal("恢复验证失败留下部分标签")
		}
	}
	if err = m.Restore(context.Background(), []Tab{tab}); err != nil {
		t.Fatal(err)
	}
	listed, err := m.List(context.Background(), "bot")
	if err != nil || len(listed) != 1 || listed[0].ID != tab.ID || listed[0].Revision != 11 || m.started {
		t.Fatal("恢复改变了身份或提前启动进程", err)
	}
	if err = m.Restore(context.Background(), nil); err == nil {
		t.Fatal("重复恢复覆盖活动标签")
	}
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.Inspect(context.Background(), func(tabs []Tab) error { close(inside); <-release; return errors.New("observer failed") })
	}()
	<-inside
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = m.List(ctx, "")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("快照观察期间仍能交错读取", err)
	}
	if err = <-done; err == nil {
		t.Fatal("观察者失败被忽略")
	}
	if err = m.CloseTab(context.Background(), Selection{Owner: "bot", ID: tab.ID, Revision: 10}); errorCode(err) != "BROWSER_TAB_CHANGED" {
		t.Fatal("旧页面版本仍可关闭", err)
	}
}

func TestBrowserStartupRecoveryStopsAtBudget(t *testing.T) {
	m, err := New(context.Background(), Options{Executable: "/missing/browser", ProfileDir: filepath.Join(t.TempDir(), "profile"), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err = m.Open(context.Background(), newTab("bot", "https://example.com")); errorCode(err) != "BROWSER_UNAVAILABLE" {
		t.Fatal("启动失败未明确返回", err)
	}
	waitBrowser(t, func() bool {
		status, err := m.Status(context.Background())
		return err == nil && status.State == "failed"
	})
	status, err := m.Status(context.Background())
	if err != nil || status.RecoveryFailures != maxRecoveries || status.Tabs != 0 {
		t.Fatal("恢复次数或标签残留错误", status, err)
	}
	if _, err = m.Open(context.Background(), newTab("bot", "https://example.com")); errorCode(err) != "BROWSER_UNAVAILABLE" {
		t.Fatal(err)
	}
	status, err = m.Status(context.Background())
	if err != nil || status.RecoveryFailures != maxRecoveries {
		t.Fatal("普通动作重置了恢复预算", status, err)
	}
}

func TestSnapshotBoundsAndFreshReferences(t *testing.T) {
	var nodes []*accessibility.Node
	for i := 0; i < 400; i++ {
		node := &accessibility.Node{BackendDOMNodeID: cdp.BackendNodeID(i + 1)}
		if err := json.Unmarshal([]byte(`{"role":{"value":"button"},"name":{"value":"中文按钮"},"value":{"value":"do-not-copy-field-value"}}`), node); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	m := &Manager{}
	tab := &entry{Tab: Tab{URL: "https://example.com", Title: "测试"}}
	first := m.renderSnapshot(tab, nodes)
	if !first.Truncated || len(strings.Split(first.Text, "\n")) > MaxSnapshotLines || len(first.Text) > MaxSnapshotBytes {
		t.Fatal("快照未截断")
	}
	if strings.Contains(first.Text, "do-not-copy-field-value") {
		t.Fatal("快照包含输入框值")
	}
	var ref string
	for value := range tab.refs {
		ref = value
		break
	}
	m.renderSnapshot(tab, nodes[:1])
	if _, err := findReference(tab, ref); errorCode(err) != "BROWSER_REF_STALE" {
		t.Fatal("旧快照引用仍生效", err)
	}
	for ref = range tab.refs {
		break
	}
	tab.epoch.Add(1)
	if _, err := findReference(tab, ref); errorCode(err) != "BROWSER_REF_STALE" {
		t.Fatal("导航后引用仍生效", err)
	}
	if !strings.HasPrefix(boundedText("中文中文", 7), "中文") || len(boundedText("中文中文", 7)) != 6 {
		t.Fatal("UTF-8 截断错误")
	}
}

func snapshotRef(t *testing.T, snapshot Snapshot, role, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`\[ref=(e[0-9]+)\] ` + regexp.QuoteMeta(role) + ` "` + regexp.QuoteMeta(name) + `"`)
	match := pattern.FindStringSubmatch(snapshot.Text)
	if len(match) != 2 {
		t.Fatalf("快照缺少目标 %s %s：%s", role, name, snapshot.Text)
	}
	return match[1]
}

func waitBrowser(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("等待浏览器状态超时")
}

func TestRealPersistentBrowser(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未设置 TOM_TEST_BROWSER_ARCHIVE，跳过真实 Chromium；浏览器交付验收必须运行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runtimeDir := t.TempDir()
	if err := provision.EnsureBrowser(ctx, runtimeDir, archive); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(runtimeDir, "profile")
	options := Options{Executable: provision.BrowserPath(runtimeDir), ProfileDir: profile, Logger: testLogger()}
	var submissions atomic.Int64
	var restoredCookie atomic.Bool
	slowStarted := make(chan struct{})
	cookieValue := testkit.Random(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := ""
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "fixture", Value: cookieValue, Path: "/", HttpOnly: true, MaxAge: 3600})
			body = `<title>中文工作页</title><form action="/result"><label>搜索<input aria-label="搜索" name="q" value="旧内容"></label><button><span>提交</span></button></form><a href="/popup" target="_blank" rel="noopener">弹出页面</a>`
		case "/result":
			submissions.Add(1)
			body = "<title>完成</title><h1>已提交</h1><p>" + html.EscapeString(r.URL.Query().Get("q")) + "</p>"
		case "/check":
			cookie, err := r.Cookie("fixture")
			restoredCookie.Store(err == nil && cookie.Value == cookieValue)
			body = "<title>登录态验证</title><h1>共享会话</h1>"
		case "/popup":
			body = "<title>弹出页</title><h1>子标签</h1>"
		case "/overlay":
			body = `<title>遮挡验证</title><form action="/result"><button style="position:absolute;left:0;top:0;width:100px;height:60px">被遮挡按钮</button></form><div style="position:absolute;left:0;top:0;width:120px;height:80px;background:white;z-index:9">遮罩</div>`
		case "/slow":
			close(slowStarted)
			<-r.Context().Done()
			return
		default:
			body = "<title>未找到</title>"
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error("测试页面写入失败", err)
		}
	}))
	defer server.Close()
	m, err := New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	tab, err := m.Open(ctx, newTab("research", server.URL+"/"))
	if err != nil {
		t.Fatal("打开页面", err)
	}
	snapshot, err := m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
	if err != nil {
		t.Fatal("读取快照", err)
	}
	search := snapshotRef(t, snapshot, "textbox", "搜索")
	if err = m.Click(ctx, Selection{Owner: "other", ID: tab.ID}, search); errorCode(err) != "BROWSER_TAB_FORBIDDEN" {
		t.Fatal("跨 Bot 点击未拒绝", err)
	}
	if err = m.Click(ctx, Selection{Owner: "research", ID: tab.ID}, search); err != nil {
		t.Fatal("点击输入框", err)
	}
	if err = m.Type(ctx, Selection{Owner: "research", ID: tab.ID}, search, "测试", true); errorCode(err) != "BROWSER_REF_STALE" {
		t.Fatal("交互后旧引用未拒绝", err)
	}
	snapshot, err = m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
	if err != nil {
		t.Fatal(err)
	}
	search = snapshotRef(t, snapshot, "textbox", "搜索")
	if err = m.Type(ctx, Selection{Owner: "research", ID: tab.ID}, search, "山巅中文搜索", true); err != nil {
		t.Fatal("输入中文", err)
	}
	if err = m.Key(ctx, Selection{Owner: "research", ID: tab.ID}, "Enter"); err != nil {
		t.Fatal("提交", err)
	}
	waitBrowser(t, func() bool { return submissions.Load() == 1 })
	waitBrowser(t, func() bool {
		snapshot, err = m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
		return err == nil && strings.Contains(snapshot.Text, "山巅中文搜索")
	})
	if strings.Contains(snapshot.Text, "旧内容") {
		t.Fatal("文本输入未清空旧内容")
	}
	picture, err := m.Screenshot(ctx, Selection{Owner: "research", ID: tab.ID}, 60)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(picture))
	if err != nil || decoded.Bounds().Dx() != ViewportWidth || decoded.Bounds().Dy() != ViewportHeight {
		t.Fatal("截图尺寸错误", err)
	}
	if _, err = m.Navigate(ctx, Selection{Owner: "research", ID: tab.ID}, server.URL+"/"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Click(ctx, Selection{Owner: "research", ID: tab.ID}, snapshotRef(t, snapshot, "button", "提交")); err != nil {
		t.Fatal("点击含子元素的按钮", err)
	}
	waitBrowser(t, func() bool { return submissions.Load() == 2 })
	if _, err = m.Navigate(ctx, Selection{Owner: "research", ID: tab.ID}, server.URL+"/overlay"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Click(ctx, Selection{Owner: "research", ID: tab.ID}, snapshotRef(t, snapshot, "button", "被遮挡按钮")); errorCode(err) != "BROWSER_TARGET_OBSCURED" {
		t.Fatal("被遮挡目标未拒绝", err)
	}
	if _, err = m.Navigate(ctx, Selection{Owner: "research", ID: tab.ID}, server.URL+"/"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = m.Snapshot(ctx, Selection{Owner: "research", ID: tab.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Click(ctx, Selection{Owner: "research", ID: tab.ID}, snapshotRef(t, snapshot, "link", "弹出页面")); err != nil {
		t.Fatal("弹出标签", err)
	}
	waitBrowser(t, func() bool { tabs, listErr := m.List(ctx, "research"); return listErr == nil && len(tabs) == 2 })
	if _, err = m.Open(ctx, newTab("writer", server.URL+"/check")); err != nil {
		t.Fatal(err)
	}
	if !restoredCookie.Load() {
		t.Fatal("Bot 未共享持久 profile 会话")
	}
	short, stop := context.WithCancel(ctx)
	openResult := make(chan error, 1)
	go func() { _, openErr := m.Open(short, newTab("research", server.URL+"/slow")); openResult <- openErr }()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("慢页面未开始加载")
	}
	stop()
	err = <-openResult
	if !errors.Is(err, context.Canceled) {
		t.Fatal("导航取消未生效", err)
	}
	tabs, err := m.List(ctx, "")
	if err != nil || len(tabs) != 3 {
		t.Fatal("取消后残留标签", err, len(tabs))
	}
	if err = m.Close(ctx); err != nil {
		t.Fatal("关闭持久浏览器", err)
	}
	m, err = New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	restoredCookie.Store(false)
	tab, err = m.Open(ctx, newTab("writer", server.URL+"/check"))
	if err != nil || !restoredCookie.Load() {
		t.Fatal("重启后登录态丢失", err)
	}
	if err = m.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	process := chromedp.FromContext(m.browser).Browser.Process()
	oldPID := process.Pid
	err = process.Kill()
	m.release()
	if err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, func() bool {
		if err := m.acquire(ctx); err != nil {
			return false
		}
		defer m.release()
		return m.alive() && chromedp.FromContext(m.browser).Browser.Process().Pid != oldPID
	})
	waitBrowser(t, func() bool {
		snapshot, err = m.Snapshot(ctx, Selection{Owner: "writer", ID: tab.ID})
		return err == nil && strings.Contains(snapshot.Text, "共享会话")
	})
	if submissions.Load() != 2 {
		t.Fatal("恢复过程重放了提交")
	}
	if err = m.CloseTab(ctx, Selection{Owner: "writer", ID: tab.ID}); err != nil {
		t.Fatal(err)
	}
	tabs, err = m.List(ctx, "")
	if err != nil || len(tabs) != 0 {
		t.Fatal("标签未关闭", err)
	}
}
