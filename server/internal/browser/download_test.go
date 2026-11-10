package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provision"
)

func TestDownloadValidationAndBoundedTracking(t *testing.T) {
	for _, input := range []DownloadInput{{}, {URL: "file:///etc/hosts"}, {URL: "https://example.com", Ref: "e1"}, {Ref: strings.Repeat("a", 33)}} {
		if input.Validate() == nil {
			t.Fatal("非法下载输入未拒绝")
		}
	}
	for _, input := range []DownloadInput{{URL: "https://example.com/file"}, {Ref: "e1"}} {
		if err := input.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"../../file", "", "12345678-1234-1234-1234-12345678901z"} {
		if validDownloadGUID(value) {
			t.Fatal("危险下载标识被接受")
		}
	}
	watch := newDownloadWatch()
	for i := 0; i <= downloadTrackingLimit; i++ {
		watch.receive(&cdpbrowser.EventDownloadWillBegin{GUID: fmt.Sprintf("12345678-1234-1234-1234-%012d", i)})
	}
	records, err := watch.snapshot()
	if len(records) != downloadTrackingLimit || errorCode(err) != "BROWSER_DOWNLOAD_GUARD_FAILED" {
		t.Fatal("下载集合未限制", err)
	}
}

func TestDownloadInitializationWaitHonorsFileStateAndDeadline(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = root.Mkdir("job", 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- waitDownloadFile(ctx, root, "job", "guid") }()
	select {
	case err := <-finished:
		t.Fatal("文件未初始化就结束等待", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err = os.WriteFile(filepath.Join(path, "job", "guid.crdownload"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err = waitDownloadFile(deadline, root, "job", "missing"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("未初始化文件没有受截止时间限制", err)
	}
	if err = os.Symlink("guid.crdownload", filepath.Join(path, "job", "linked.crdownload")); err != nil {
		t.Fatal(err)
	}
	if err = waitDownloadFile(ctx, root, "job", "linked"); errorCode(err) != "BROWSER_DOWNLOAD_GUARD_FAILED" {
		t.Fatal("初始化校验接受了符号链接", err)
	}
}

func TestDownloadFileValidationCleanupAndGuardFailure(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	job := "job-" + ids.New()
	if err = root.Mkdir(job, 0700); err != nil {
		t.Fatal(err)
	}
	guid := "12345678-1234-1234-1234-123456789012"
	record := downloadRecord{begin: cdpbrowser.EventDownloadWillBegin{GUID: guid, SuggestedFilename: "report.txt"}, progress: cdpbrowser.EventDownloadProgress{ReceivedBytes: 3, State: cdpbrowser.DownloadProgressStateCompleted}}
	name := filepath.Join(base, job, guid)
	if err = os.WriteFile(name, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("文件发布失败")
	if err = consumeDownload(ctx, root, job, record, func(context.Context, string, io.Reader) error { return failure }); !errors.Is(err, failure) {
		t.Fatal("发布失败被掩盖", err)
	}
	record.progress.ReceivedBytes = 4
	if err = consumeDownload(ctx, root, job, record, func(context.Context, string, io.Reader) error { t.Error("不完整文件被发布"); return nil }); errorCode(err) != "BROWSER_DOWNLOAD_INVALID" {
		t.Fatal("下载长度未核对", err)
	}
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err = os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	if err = consumeDownload(ctx, root, job, record, func(context.Context, string, io.Reader) error { t.Error("符号链接文件被发布"); return nil }); errorCode(err) != "BROWSER_DOWNLOAD_INVALID" {
		t.Fatal("下载链接未拒绝", err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	m, err := New(ctx, Options{Executable: "/missing/browser", ProfileDir: profile, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	staging := filepath.Join(profile, "tom-downloads")
	if err = os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Dir(outside), filepath.Join(staging, "job-"+ids.New())); err != nil {
		t.Fatal(err)
	}
	if err = m.clearDownloadStaging(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatal("恢复清理越出暂存目录", err)
	}
	m.stopUnsafeDownload()
	if err = m.ready(ctx); errorCode(err) != "BROWSER_DOWNLOAD_GUARD_FAILED" {
		t.Fatal("下载保护故障没有阻断后续动作", err)
	}
	session := &navigationSession{failure: failure}
	if err = closeBrowserDownloads(ctx, session, newDownloadWatch(), nil); !errors.Is(err, failure) {
		t.Fatal("拒绝下载的协议失败被忽略", err)
	}
}

func TestRealBrowserDownloadsPreserveSessionCancelAndDoNotReplay(t *testing.T) {
	archive := os.Getenv("TOM_TEST_BROWSER_ARCHIVE")
	if archive == "" {
		t.Skip("未提供浏览器归档，跳过真实下载验收")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := provision.EnsureBrowser(ctx, root, archive); err != nil {
		t.Fatal(err)
	}
	var requests, posts atomic.Int64
	var blockedRequests atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { blockedRequests.Add(1); w.WriteHeader(204) }))
	defer blocked.Close()
	slowStarted, slowStopped := make(chan struct{}, 1), make(chan struct{}, 1)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "tom_download_fixture", Value: "active", Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := io.WriteString(w, `<title>下载测试</title><form method="post" action="/file"><button>下载报表</button></form>`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, blocked.URL+"/file", 302)
			return
		}
		if r.URL.Path == "/page" {
			w.Header().Set("Content-Type", "text/html")
			if _, err := io.WriteString(w, "<title>不是下载</title>"); err != nil {
				t.Error(err)
			}
			return
		}
		requests.Add(1)
		cookie, err := r.Cookie("tom_download_fixture")
		if err != nil || cookie.Value != "active" {
			t.Error("下载没有携带浏览器登录态")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
		if r.URL.Path == "/large" {
			w.Header().Set("Content-Length", fmt.Sprint(MaxDownloadBytes+1))
			w.WriteHeader(200)
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/slow" {
			w.WriteHeader(200)
			if _, err := io.WriteString(w, "start"); err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
			slowStarted <- struct{}{}
			<-r.Context().Done()
			slowStopped <- struct{}{}
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
		}
		if _, err := io.WriteString(w, "下载内容：中文\x00binary"); err != nil {
			t.Error(err)
		}
	}))
	defer host.Close()
	m, err := New(ctx, Options{Executable: provision.BrowserPath(root), ProfileDir: filepath.Join(root, "profile"), Logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})), AuthorizeNavigation: func(ctx context.Context, request Navigation) error {
		if strings.HasPrefix(request.URL, host.URL+"/") || request.URL == host.URL {
			return nil
		}
		return domain.NewError(403, "BROWSER_DOMAIN_APPROVAL_REQUIRED", "Origin not approved.")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	tab, err := m.Open(ctx, newTab("writer", host.URL))
	if err != nil {
		t.Fatal(err)
	}
	selected := Selection{Owner: "writer", ID: tab.ID}
	consumed := 0
	consume := func(ctx context.Context, name string, source io.Reader) error {
		content, err := io.ReadAll(source)
		if err != nil {
			return err
		}
		if name != "report.txt" || string(content) != "下载内容：中文\x00binary" {
			t.Error("下载内容或名称不一致")
		}
		consumed++
		return nil
	}
	if err = m.Download(ctx, selected, DownloadInput{URL: host.URL + "/file"}, consume); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	ref := ""
	for _, line := range strings.Split(snapshot.Text, "\n") {
		if strings.Contains(line, "下载报表") && strings.Contains(line, "[ref=") {
			ref = strings.Split(strings.Split(line, "[ref=")[1], "]")[0]
			break
		}
	}
	if ref == "" {
		t.Fatal("没有下载按钮引用", snapshot.Text)
	}
	selected.Revision = snapshot.Tab.Revision
	if err = m.Download(ctx, selected, DownloadInput{Ref: ref}, consume); err != nil {
		t.Fatal(err)
	}
	if consumed != 2 || posts.Load() != 1 || requests.Load() != 2 {
		t.Fatal("下载被重放", consumed, posts.Load(), requests.Load())
	}
	selected.Revision = 0
	if err = m.Download(ctx, selected, DownloadInput{URL: host.URL + "/large"}, consume); errorCode(err) != "BROWSER_DOWNLOAD_TOO_LARGE" {
		t.Fatal("超限下载未取消", err)
	}
	operation, stop := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() { finished <- m.Download(operation, selected, DownloadInput{URL: host.URL + "/slow"}, consume) }()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("下载没有启动")
	}
	stop()
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("取消未传播", err)
	}
	cancelErr := err
	select {
	case <-slowStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后网络仍在下载")
	}
	entries, err := os.ReadDir(filepath.Join(root, "profile", "tom-downloads"))
	if err != nil || len(entries) != 0 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatal("下载暂存未清理", err, names, cancelErr)
	}
	if consumed != 2 {
		t.Fatal("不完整下载发布了文件")
	}
	if state, err := m.Status(ctx); err != nil || state.State != "running" {
		t.Fatal("正常取消破坏浏览器", state, err)
	}
	if err = m.Download(ctx, selected, DownloadInput{URL: host.URL + "/redirect"}, consume); err == nil || blockedRequests.Load() != 0 {
		t.Fatal("下载跳转绕过来源许可", err)
	}
	if m.downloadFailed.Load() {
		t.Fatal("正常来源阻断不应破坏下载保护", err)
	}
	if _, err = m.Navigate(ctx, selected, host.URL); err != nil {
		t.Fatal(err)
	}
	if err = m.Download(ctx, selected, DownloadInput{URL: host.URL + "/page"}, consume); errorCode(err) != "BROWSER_DOWNLOAD_NOT_STARTED" {
		t.Fatal("普通网页被当作下载", err)
	}
	tabs, err := m.List(ctx, "writer")
	if err != nil || tabs[0].URL != host.URL+"/page" {
		t.Fatal("下载失败后仍保留旧地址", err)
	}
}
