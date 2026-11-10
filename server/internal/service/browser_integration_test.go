package service

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/provision"
)

func TestRealBrowserServicePersistenceAndActions(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供 TOM_TEST_BROWSER_ARCHIVE，跳过真实 Chromium 服务验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	fixture, _, bot, _ := browserServiceFixture(t)
	if err := fixture.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	dependencies := BrowserDependencies{DB: fixture.db, Bots: fixture.bots, Events: fixture.events, Logger: fixture.logger, Options: browser.Options{Executable: provision.BrowserPath(root), ProfileDir: filepath.Join(root, "profile")}}
	var submissions atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<title>持久标签</title><h1>浏览器服务</h1><form method="post" action="/submitted"><label>名称<input name="name"></label><button>提交</button></form><a href="/popup" target="_blank">打开弹窗</a>`
		if r.URL.Path == "/submitted" && r.Method == "POST" {
			submissions.Add(1)
			http.Redirect(w, r, "/done", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/done" {
			body = "<title>已完成</title><h1>提交完成</h1>"
		}
		if r.URL.Path == "/popup" {
			body = "<title>子页面</title><h1>持久弹窗</h1>"
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer host.Close()
	s, err := NewBrowser(ctx, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err = s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tab, err := s.Open(ctx, BrowserOpenInput{BotID: bot.ID, URL: host.URL}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx, tab.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`\[ref=(e[0-9]+)\] textbox "名称"`).FindStringSubmatch(snapshot.Text)
	if len(ref) != 2 {
		t.Fatal("快照缺少输入引用")
	}
	tab, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "type", Ref: ref[1], Text: "持久浏览器验收"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "key", Key: "Enter"}, nil, "user"); browserCode(err) != "BROWSER_TAB_CHANGED" {
		t.Fatal("真实页面版本未拒绝旧动作", err)
	}
	snapshot, err = s.Snapshot(ctx, tab.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "key", Key: "Enter"}, nil, "user"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		snapshot, err = s.Snapshot(ctx, tab.ID, nil)
		if err == nil && strings.Contains(snapshot.Text, "提交完成") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("页面未完成提交", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	picture, err := s.Screenshot(ctx, tab.ID, nil, 60)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(picture))
	if err != nil || decoded.Bounds().Dx() != browser.ViewportWidth {
		t.Fatal("真实截图格式错误", err)
	}
	tab, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "navigate", URL: host.URL}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.Snapshot(ctx, tab.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref = regexp.MustCompile(`\[ref=(e[0-9]+)\] link "打开弹窗"`).FindStringSubmatch(snapshot.Text)
	if len(ref) != 2 {
		t.Fatal("快照缺少弹窗链接")
	}
	if _, err = s.Apply(ctx, tab.ID, BrowserAction{BrowserVersion: browserVersion(snapshot.Tab), Action: "click", Ref: ref[1]}, nil, "user"); err != nil {
		t.Fatal(err)
	}
	var saved []BrowserTab
	deadline = time.Now().Add(8 * time.Second)
	for {
		saved, err = s.List(ctx, bot.ID)
		if err == nil && len(saved) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("弹窗未持久化", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = NewBrowser(ctx, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, before := range saved {
		snapshot, err = s.Snapshot(ctx, before.ID, nil)
		if err != nil || snapshot.Tab.BotID != bot.ID || snapshot.Tab.Controller != "user" || snapshot.Tab.PageRevision <= before.PageRevision {
			t.Fatal("服务重建未保留标签与递增版本", err)
		}
		if err = s.CloseTab(ctx, before.ID, browserVersion(snapshot.Tab), nil, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if submissions.Load() != 1 {
		t.Fatal("恢复重放了表单提交")
	}
}
