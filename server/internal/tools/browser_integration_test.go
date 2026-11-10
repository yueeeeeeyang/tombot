package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	"io.yueyang/tom/internal/service"
)

func TestRealBrowserToolApprovalAndBlockedRedirect(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供 TOM_TEST_BROWSER_ARCHIVE，跳过真实浏览器工具审批验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	e, bot, run := fixtureAt(t, "loose", workspaceRoot)
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	if err := e.browser.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	browsing, err := service.NewBrowser(ctx, service.BrowserDependencies{DB: e.db, Bots: service.NewBots(e.db, e.events, workspaceRoot), Events: e.events, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Options: browser.Options{Executable: provision.BrowserPath(root), ProfileDir: filepath.Join(root, "profile")}})
	if err != nil {
		t.Fatal(err)
	}
	e.browser = browsing
	defer func() {
		if err := browsing.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	var submissions, hits, landings, downloads atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/landing" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		landings.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := io.WriteString(w, "<title>第二来源</title><h1>已获准</h1>"); err != nil {
			t.Error(err)
		}
	}))
	defer other.Close()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			downloads.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
			if _, err := io.WriteString(w, "审批后的原生下载"); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/landing", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<title>浏览器工具</title><form action="/submitted" method="post"><label>名称<input name="name"></label><button>提交</button></form>`
		if r.URL.Path == "/submitted" {
			submissions.Add(1)
			body = "<title>已提交</title><h1>提交完成</h1>"
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer host.Close()
	result := approveBrowserTool(t, e, bot, run, map[string]any{"action": "open", "url": host.URL}, nil)
	var tab service.BrowserTab
	if err = json.Unmarshal([]byte(result.Content), &tab); err != nil || result.IsError {
		t.Fatal("工具打开失败", err, result.Content)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "snapshot", "id": tab.ID})
	var snapshot service.BrowserSnapshot
	if err = json.Unmarshal([]byte(result.Content), &snapshot); err != nil || result.IsError {
		t.Fatal("工具快照失败", err)
	}
	ref := regexp.MustCompile(`\[ref=(e[0-9]+)\] textbox "名称"`).FindStringSubmatch(snapshot.Text)
	if len(ref) != 2 {
		t.Fatal("缺少输入框引用")
	}
	result = approveBrowserTool(t, e, bot, run, map[string]any{"action": "type", "id": tab.ID, "ref": ref[1], "text": "真实工具输入"}, nil)
	if result.IsError {
		t.Fatal(result.Content)
	}
	result = approveBrowserTool(t, e, bot, run, map[string]any{"action": "key", "id": tab.ID, "key": "Enter"}, nil)
	if result.IsError {
		t.Fatal(result.Content)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		result = execute(t, e, bot, run, "browser", map[string]any{"action": "snapshot", "id": tab.ID})
		if !result.IsError && strings.Contains(result.Content, "提交完成") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("实际页面未完成提交", result.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "screenshot", "id": tab.ID})
	if result.IsError || !strings.Contains(result.Content, ".jpg") {
		t.Fatal("截图未保存为产物", result.Content)
	}
	result = approveBrowserTool(t, e, bot, run, map[string]any{"action": "download", "id": tab.ID, "url": host.URL + "/file"}, func(a service.ApprovalSummary) {
		if downloads.Load() != 0 || a.ActionClass != "browser.interact" {
			t.Fatal("下载在审批前发出请求或没有逐次确认")
		}
	})
	var downloaded service.BrowserDownload
	if err = json.Unmarshal([]byte(result.Content), &downloaded); err != nil || result.IsError || downloads.Load() != 1 {
		t.Fatal("真实下载工具未完成或重放", err, result.Content)
	}
	content, err := os.ReadFile(filepath.Join(workspaceRoot, "bots", bot.ID, filepath.FromSlash(downloaded.File.Path)))
	if err != nil || string(content) != "审批后的原生下载" {
		t.Fatal("原生下载未发布到 Bot 工作区", err)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "open", "id": tab.ID, "url": host.URL + "/redirect"})
	var denied browserToolFailure
	if err = json.Unmarshal([]byte(result.Content), &denied); err != nil || !result.IsError || denied.Tab.BlockedNavigation == nil || denied.Tab.BlockedNavigation.URL != other.URL+"/landing" || hits.Load() != 0 {
		t.Fatal("重定向阻断信息未回灌或目标已收到请求", err, result.Content)
	}
	// 拦截会进入浏览器错误页；新提案须先查看当前页面，不能使用被阻断导航返回时的旧版本。
	deadline = time.Now().Add(8 * time.Second)
	backoff := 20 * time.Millisecond
	for {
		result = execute(t, e, bot, run, "browser", map[string]any{"action": "snapshot", "id": tab.ID})
		if !result.IsError {
			if !strings.Contains(result.Content, "BROWSER_DOMAIN_APPROVAL_REQUIRED") {
				t.Fatal("快照丢失导航阻断原因", result.Content)
			}
			break
		}
		if !strings.Contains(result.Content, "BROWSER_DOCUMENT_CHANGED") || time.Now().After(deadline) {
			t.Fatal("阻断后无法查看当前页面", result.Content)
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 200*time.Millisecond)
	}
	result = approveBrowserTool(t, e, bot, run, map[string]any{"action": "open", "id": tab.ID, "url": other.URL + "/landing"}, func(a service.ApprovalSummary) {
		if hits.Load() != 0 {
			t.Fatal("新源审批前产生请求")
		}
	})
	if result.IsError || landings.Load() != 1 || submissions.Load() != 1 {
		t.Fatal("批准后没有单次导航或重放了提交", result.Content, landings.Load(), submissions.Load())
	}
}
