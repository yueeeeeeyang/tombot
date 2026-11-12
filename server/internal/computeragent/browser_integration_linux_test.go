//go:build linux && agentintegration && !cgo

package computeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

func requireBrowserContainer(t *testing.T) {
	t.Helper()
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" || taskprocess.CheckController() != nil {
		t.Fatal("共享浏览器验收要求纯 Go 专用受限容器")
	}
	if err := os.Mkdir(computer.BrowserRuntimeRoot, 0711); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
}

func TestContainerBrowserDirectory(t *testing.T) {
	requireBrowserContainer(t)
	request, _, _ := browserFixture(t)
	directory, err := newBrowserDirectory(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	other := request
	other.Group.ID = ids.New()
	if _, err := newBrowserDirectory(other); err == nil {
		t.Fatal("允许同时持有两个共享 profile 实例")
	}
	saved := filepath.Join(computer.BrowserDirectory, "retained-fixture")
	if err := os.WriteFile(saved, []byte("保留共享状态"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(saved); err != nil {
			t.Error(err)
		}
	})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "retained"), []byte("不得递归删除"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(request.RuntimeDirectory(), "outside")); err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "retained")); err != nil {
		t.Fatal("运行目录清理越过链接", err)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatal("运行目录清理误删 profile", err)
	}
	if err := os.Mkdir(other.RuntimeDirectory(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := newBrowserDirectory(other); err == nil {
		t.Fatal("覆盖已有运行实例")
	}
	if err := os.Remove(other.RuntimeDirectory()); err != nil {
		t.Fatal(err)
	}
	next, err := newBrowserDirectory(other)
	if err != nil {
		t.Fatal("失败启动未释放独占锁", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContainerBrowserRunnerRejectsFakeMembership(t *testing.T) {
	requireBrowserContainer(t)
	request, options, _ := browserFixture(t)
	directory, err := newBrowserDirectory(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	}()
	membership, err := os.CreateTemp(t.TempDir(), "not-cgroup")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := membership.Close(); err != nil {
			t.Error(err)
		}
	}()
	process, err := newBrowserProcess()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	service, err := process.start(ctx, request, membership, options, prepareBrowserProcess)
	if err == nil || service != nil {
		t.Fatal("实际浏览器引导接受伪造服务组")
	}
	if err := process.wait(ctx); err == nil || !process.finished() {
		t.Fatal("实际拒绝未完整回收", err)
	}
	if stat, err := membership.Stat(); err != nil || stat.Size() != 0 {
		t.Fatal("引导改写了伪造内核入口", err)
	}
}

func TestContainerManagedChromeProfileAndProxy(t *testing.T) {
	requireBrowserContainer(t)
	request, options, sessions := browserFixture(t)
	options.CallTimeout, options.ShutdownTimeout = 5*time.Second, 5*time.Second
	var directCalls atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { directCalls.Add(1) }))
	defer direct.Close()
	var proxyCalls atomic.Int32
	proxy := &http.Server{ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || !r.URL.IsAbs() || r.URL.Host != strings.TrimPrefix(direct.URL, "http://") {
				http.Error(w, "测试代理拒绝其他目标", http.StatusForbidden)
				return
			}
			proxyCalls.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			account := "匿名"
			if cookie, err := r.Cookie("fixture_account"); err == nil && cookie.Value == "shared" {
				account = "共享账号"
			}
			if r.URL.Path == "/login" {
				http.SetCookie(w, &http.Cookie{Name: "fixture_account", Value: "shared", MaxAge: 3600, Path: "/", HttpOnly: true})
				account = "共享账号"
			}
			if _, err := fmt.Fprintf(w, "<!doctype html><html><head><title>%s</title></head><body><h1>%s</h1><input autofocus style='font-size:24px;width:650px'></body></html>", account, account); err != nil {
				t.Error("测试站点响应失败", err)
			}
		})}
	listener, err := net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxy.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-proxyDone; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	store := &memoryBrowserStore{}
	var revision int64
	launch := func() (*browserProcess, *browserDirectory, *sharedbrowser.Service, *BrowserController, BrowserGrant) {
		t.Helper()
		request.Group.ID = ids.New()
		directory, err := newBrowserDirectory(request)
		if err != nil {
			t.Fatal(err)
		}
		process, err := newBrowserProcess()
		if err != nil {
			t.Fatal(err)
		}
		// 仅测试夹具直接降权，用真实 Chrome 验证进程监督；正式启动始终走已封存的入组引导。
		prepare := func(request taskprocess.BrowserRequest, _, _, commands, responses *os.File) (*exec.Cmd, io.Closer, error) {
			command := exec.Command(taskprocess.BrowserProgram, "--headless", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-gpu", "--disable-quic", "--remote-debugging-pipe", "--proxy-server=http://127.0.0.1:3128", "--proxy-bypass-list=<-loopback>", "--user-data-dir="+computer.BrowserDirectory, "about:blank")
			command.Env = []string{"PATH=/usr/bin:/bin", "LANG=zh_CN.UTF-8", "HOME=" + computer.BrowserDirectory, "XDG_RUNTIME_DIR=" + request.RuntimeDirectory(), "TMPDIR=" + request.RuntimeDirectory()}
			command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: computer.BrowserUID, Gid: computer.DesktopSharedGID}, Setpgid: true}
			command.ExtraFiles = []*os.File{commands, responses}
			return command, nil, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		var service *sharedbrowser.Service
		factory := &fakeBrowserFactory{prepare: func(scope *fakeBrowserScope) {
			scope.start = func(ctx context.Context, request taskprocess.BrowserRequest, active sharedbrowser.Options) (BrowserService, error) {
				var err error
				service, err = process.start(ctx, request, nil, active, prepare)
				return service, err
			}
			scope.drain = process.close
		}}
		identity := Identity{UserID: sessions[0].UserID, ComputerID: sessions[0].ComputerID, NodeID: sessions[0].NodeID, Generation: sessions[0].Generation}
		browsers, err := NewBrowserController(ctx, identity, store, factory, Options{CallTimeout: 10 * time.Second, CleanupTimeout: 5 * time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, options.Authorize, options.Logger, func() { t.Error("真实 Chrome 监督失控") })
		if err != nil {
			t.Fatal(err)
		}
		revision++
		grant := BrowserGrant{Identity: identity, Revision: revision, Request: request, Limits: BrowserLimits{Width: options.Width, Height: options.Height, MaximumWindows: options.MaximumWindows, MaximumPerSession: options.MaximumPerSession, MaximumWindowRecords: options.MaximumWindowRecords, MaximumEvents: options.MaximumEvents, CallTimeoutMS: int(options.CallTimeout / time.Millisecond), DialogTimeoutMS: int(options.DialogTimeout / time.Millisecond), ShutdownTimeoutMS: int(options.ShutdownTimeout / time.Millisecond)}}
		_, err = browsers.startAt(ctx, grant, probeFiles(t), time.Now().Add(10*time.Second))
		if err != nil {
			t.Fatal("真实 Chrome 进程监督未就绪", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := browsers.Shutdown(ctx); err != nil {
				t.Error(err)
			}
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
		return process, directory, service, browsers, grant
	}
	navigate := func(service *sharedbrowser.Service, address sharedbrowser.Address, sequence int64, path string) {
		t.Helper()
		if _, err := service.Execute(t.Context(), address, sharedbrowser.Command{Sequence: sequence, Kind: "navigate", URL: direct.URL + path}); err != nil {
			t.Fatal(err)
		}
	}
	waitTitle := func(service *sharedbrowser.Service, session computer.ExecutionSession) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			windows, err := service.List(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if len(windows) == 1 && windows[0].Title == "共享账号" {
				return
			}
			if time.Now().After(deadline) {
				if len(windows) == 1 && os.Getenv("TOM_BROWSER_PROCESS_REPORT") != "" {
					encoded, err := service.Screenshot(t.Context(), sharedbrowser.Address{Session: session, WindowID: windows[0].ID})
					if err == nil {
						err = os.WriteFile(filepath.Join(os.Getenv("TOM_BROWSER_PROCESS_REPORT"), "failed-page.png"), encoded, 0600)
					}
					if err != nil {
						t.Error("保存测试页面诊断失败", err)
					}
				}
				t.Fatal("账号状态没有共享或保留", windows, proxyCalls.Load(), directCalls.Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	process, directory, service, browsers, grant := launch()
	windows := liveWindowGateways(t, browsers, grant, options, sessions)
	addresses := []sharedbrowser.Address{{Session: sessions[0], WindowID: ids.New()}, {Session: sessions[1], WindowID: ids.New()}}
	for _, address := range addresses {
		if _, err := service.Open(t.Context(), address); err != nil {
			t.Fatal(err)
		}
	}
	navigate(service, addresses[0], 1, "/login")
	waitTitle(service, sessions[0])
	navigate(service, addresses[1], 1, "/state")
	waitTitle(service, sessions[1])
	for index, address := range addresses {
		if _, err := service.Execute(t.Context(), address, sharedbrowser.Command{Sequence: 2, Kind: "type", Text: fmt.Sprintf("机器人 %d 的独立输入", index+1)}); err != nil {
			t.Fatal(err)
		}
		encoded, err := service.Screenshot(t.Context(), address)
		if err != nil {
			t.Fatal(err)
		}
		if report := os.Getenv("TOM_BROWSER_PROCESS_REPORT"); report != "" {
			if err := os.WriteFile(filepath.Join(report, fmt.Sprintf("browser-%d.png", index+1)), encoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index, entry := range windows {
		status, encoded, err := requestWindow(t.Context(), sessions[index], sessions[index].UID, "GET", "/v2/state", "")
		var result struct {
			Windows []sharedbrowser.Window `json:"windows"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(encoded, &result) != nil || len(result.Windows) != 1 || result.Windows[0].ID != addresses[index].WindowID || result.Windows[0].Title != "共享账号" {
			t.Fatal("实际窗口入口未返回所属 Chrome 页面", status, err)
		}
		if entry.instance != browsers.available.Load() {
			t.Fatal("窗口未固定原共享服务")
		}
		if err := entry.UseBrowser(t.Context(), sessions[index], func(ctx context.Context, current sharedbrowser.Browser) error {
			if current != service {
				return errors.New("直接调用未固定原 Chrome")
			}
			before, err := current.Screenshot(ctx, addresses[index])
			if err != nil {
				return err
			}
			if _, err := current.Execute(ctx, addresses[index], sharedbrowser.Command{Sequence: 3, Kind: "key", Key: "A", Modifiers: 2}); err != nil {
				return err
			}
			if _, err := current.Execute(ctx, addresses[index], sharedbrowser.Command{Sequence: 4, Kind: "type", Text: fmt.Sprintf("直接驱动 Chrome %d", index+1)}); err != nil {
				return err
			}
			after, err := current.Screenshot(ctx, addresses[index])
			if err != nil {
				return err
			}
			if bytes.Equal(before, after) {
				return errors.New("直接调用没有改变所属真实页面")
			}
			foreign := addresses[index]
			foreign.Session = sessions[1-index]
			if _, err := current.Screenshot(ctx, foreign); !errors.Is(err, sharedbrowser.ErrDenied) {
				return errors.New("直接调用允许跨 Bot 页面访问")
			}
			return nil
		}); err != nil {
			t.Fatal("真实 Chrome 直接调用失败", err)
		}
	}
	windows[0].Revoke()
	select {
	case <-windows[0].retired:
	case <-time.After(6 * time.Second):
		t.Fatal("独立撤销未关闭真实 Chrome 页面")
	}
	if remaining, err := service.List(t.Context(), sessions[0]); err != nil || len(remaining) != 0 {
		t.Fatal("关闭入口后遗留甲的 Chrome 页面", err)
	}
	if err := windows[0].Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Screenshot(t.Context(), addresses[1]); err != nil {
		t.Fatal("一个 Bot 回收影响共享浏览器", err)
	}
	select {
	case <-process.done:
		t.Fatal("单个 Bot 结束关闭了 Chrome")
	default:
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := windows[1].Close(ctx); err != nil {
		t.Fatal("乙的页面未完成回收", err)
	}
	if _, err := browsers.Close(ctx, grant); err != nil {
		t.Fatal("Chrome 未正常关闭", err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, restarted, _, _ := launch()
	address := sharedbrowser.Address{Session: sessions[1], WindowID: ids.New()}
	if _, err := restarted.Open(t.Context(), address); err != nil {
		t.Fatal(err)
	}
	navigate(restarted, address, 1, "/state")
	waitTitle(restarted, sessions[1])
	if directCalls.Load() != 0 || proxyCalls.Load() < 3 {
		t.Fatal("Chrome 绕过了固定代理", directCalls.Load(), proxyCalls.Load())
	}
}
