package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provision"
)

type navigationSession struct {
	cdp.Session
	methods []string
	failure error
}

func (s *navigationSession) Call(_ context.Context, method string, _, _ any) error {
	s.methods = append(s.methods, method)
	return s.failure
}

func TestNavigationGuardDenialProtocolFailureAndCancellation(t *testing.T) {
	policyFailure := errors.New("授权存储不可用")
	protocolFailure := &cdproto.Error{Code: -32602, Message: "Invalid parameters"}
	canceled := &cdproto.Error{Code: -32602, Message: "Invalid InterceptionId."}
	for _, test := range []struct {
		name       string
		decision   error
		protocol   error
		wantMethod string
		wantCode   string
		wantError  error
	}{
		{"允许", nil, nil, fetch.ContinueRequest.Method, "", nil},
		{"拒绝", domain.NewError(403, "TOOL_DENIED", "Denied."), nil, fetch.FailRequest.Method, "TOOL_DENIED", nil},
		{"授权故障", policyFailure, nil, fetch.FailRequest.Method, "BROWSER_NAVIGATION_POLICY_FAILED", policyFailure},
		{"放行协议故障", nil, protocolFailure, fetch.ContinueRequest.Method, "", protocolFailure},
		{"已取消的放行", nil, canceled, fetch.ContinueRequest.Method, "", nil},
		{"已取消的拒绝", domain.NewError(403, "TOOL_DENIED", "Denied."), canceled, fetch.FailRequest.Method, "TOOL_DENIED", nil},
		{"取消不掩盖授权故障", policyFailure, canceled, fetch.FailRequest.Method, "BROWSER_NAVIGATION_POLICY_FAILED", policyFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &navigationSession{failure: test.protocol}
			g := newNavigationGuard(context.Background(), nil, func(_ context.Context, request Navigation) error {
				if request.TabID != "tab" || request.Owner != "bot" || request.Method != "POST" {
					t.Fatal("授权请求身份缺失")
				}
				return test.decision
			}, testLogger(), func() {}, func() {})
			g.session = session
			g.frames["frame"] = navigationOwner{tabID: "tab", owner: "bot"}
			err := g.handle(context.Background(), &fetch.EventRequestPaused{RequestID: "request", FrameID: "frame", Request: &network.Request{URL: "https://example.com/submit", Method: "POST"}})
			if !errors.Is(err, test.wantError) || len(session.methods) != 1 || session.methods[0] != test.wantMethod {
				t.Fatal("拦截决策未按预期处理", err, session.methods)
			}
			blocked := g.denied("tab")
			if test.wantCode == "" && blocked != nil || test.wantCode != "" && (blocked == nil || blocked.Code != test.wantCode || blocked.Method != "POST") {
				t.Fatal("阻断原因未保留", blocked)
			}
		})
	}
}

func TestNavigationGuardFailureStopsOnceAndHonorsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stops := 0
	g := newNavigationGuard(ctx, nil, nil, testLogger(), func() { stops++ }, func() {})
	for i := 0; i < navigationFrameLimit; i++ {
		g.frames[cdp.FrameID(strings.Repeat("x", i+1))] = navigationOwner{tabID: "tab", owner: "bot"}
	}
	g.attached("new", "x")
	g.fail("BROWSER_NAVIGATION_GUARD_FAILED")
	if stops != 1 {
		t.Fatal("容量故障未停止或重复停止进程", stops)
	}
	if _, ok := g.lookup("new"); ok {
		t.Fatal("超量文档仍被登记")
	}
	cancel()
	g = newNavigationGuard(ctx, nil, nil, testLogger(), func() { stops++ }, func() {})
	g.fail("BROWSER_NAVIGATION_GUARD_FAILED")
	if stops != 1 {
		t.Fatal("旧进程取消后的事件污染了恢复状态")
	}
}

func TestRealBrowserNavigationGuard(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供 TOM_TEST_BROWSER_ARCHIVE，跳过真实导航保护验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(204) }))
	defer blocked.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, blocked.URL+"/redirected", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := `<title>导航保护</title><form method="post" action="` + blocked.URL + `/submit"><button>提交表单</button></form><a href="` + blocked.URL + `/popup" target="_blank">新标签</a>`
		if r.URL.Path == "/frame" {
			body += `<iframe src="` + blocked.URL + `/frame"></iframe>`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer allowed.Close()
	var mu sync.Mutex
	var policyFailed atomic.Bool
	observed := []Navigation{}
	m, err := New(ctx, Options{Executable: provision.BrowserPath(root), ProfileDir: filepath.Join(root, "profile"), Logger: testLogger(), AuthorizeNavigation: func(ctx context.Context, request Navigation) error {
		if policyFailed.Load() {
			return errors.New("授权存储不可用")
		}
		mu.Lock()
		observed = append(observed, request)
		mu.Unlock()
		if strings.HasPrefix(request.URL, allowed.URL+"/") || request.URL == allowed.URL {
			return nil
		}
		return domain.NewError(403, "BROWSER_DOMAIN_APPROVAL_REQUIRED", "Request approval for the new origin.")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	tab, err := m.Open(ctx, newTab("bot", allowed.URL))
	if err != nil {
		t.Fatal("获准页面无法打开", err)
	}
	selected := Selection{Owner: "bot", ID: tab.ID}
	result, err := m.Navigate(ctx, selected, allowed.URL+"/redirect")
	if errorCode(err) != "BROWSER_DOMAIN_APPROVAL_REQUIRED" || result.BlockedNavigation == nil || result.BlockedNavigation.URL != blocked.URL+"/redirected" {
		t.Fatal("重定向没有在发送前阻断", err, result.BlockedNavigation)
	}
	if _, err = m.Navigate(ctx, selected, allowed.URL); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Click(ctx, selected, snapshotRef(t, snapshot, "link", "新标签")); err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, request := range observed {
			if request.URL == blocked.URL+"/popup" && request.TabID == tab.ID && request.Owner == "bot" {
				return true
			}
		}
		return false
	})
	tabs, err := m.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, popup := range tabs {
		if popup.ID != tab.ID {
			if err = m.CloseTab(ctx, Selection{Owner: popup.BotID, ID: popup.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = m.Navigate(ctx, selected, allowed.URL); err != nil {
		t.Fatal(err)
	}
	snapshot, err = m.Snapshot(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Click(ctx, selected, snapshotRef(t, snapshot, "button", "提交表单")); err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, request := range observed {
			if request.URL == blocked.URL+"/submit" && request.Method == "POST" {
				return true
			}
		}
		return false
	})
	if _, err = m.Navigate(ctx, selected, allowed.URL+"/frame"); err != nil {
		t.Fatal(err)
	}
	waitBrowser(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, request := range observed {
			if request.URL == blocked.URL+"/frame" && request.TabID == tab.ID {
				return true
			}
		}
		return false
	})
	opened, err := m.Open(ctx, newTab("bot", blocked.URL+"/direct"))
	if errorCode(err) != "BROWSER_DOMAIN_APPROVAL_REQUIRED" || opened.ID == "" || opened.BlockedNavigation == nil {
		t.Fatal("打开被拒地址未保留明确结果", err)
	}
	if hits.Load() != 0 {
		t.Fatal("未批准的请求已到达目标服务", hits.Load())
	}
	if err = m.CloseTab(ctx, Selection{Owner: "bot", ID: opened.ID}); err != nil {
		t.Fatal(err)
	}
	policyFailed.Store(true)
	if _, err = m.Navigate(ctx, selected, allowed.URL); err == nil {
		t.Fatal("授权存储故障仍返回导航成功")
	}
	waitBrowser(t, func() bool {
		status, err := m.Status(ctx)
		return err == nil && status.State == "failed"
	})
	if _, err = m.Open(ctx, newTab("bot", allowed.URL)); errorCode(err) != "BROWSER_NAVIGATION_GUARD_FAILED" {
		t.Fatal("保护故障后仍允许隐式重启", err)
	}
	policyFailed.Store(false)
	if err = m.Restart(ctx); err != nil {
		t.Fatal("显式恢复保护失败", err)
	}
	if _, err = m.Navigate(ctx, selected, allowed.URL); err != nil {
		t.Fatal("恢复后获准导航失败", err)
	}
}
