//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type liveBrowser struct {
	service *Service
	process *exec.Cmd
	done    chan error
}

// launchTestBrowser 在临时容器中直接降权；生产独立服务 cgroup 与监督器接入另行验收。
func launchTestBrowser(t *testing.T, profile string, options Options) *liveBrowser {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("TOM_SHARED_BROWSER_TEST") != "1" {
		t.Fatal("必须在专用受限容器内验证共享浏览器")
	}
	uploads, err := NewUploadStore()
	if err != nil {
		t.Fatal(err)
	}
	options.Uploads = uploads
	t.Cleanup(func() {
		if err := uploads.Close(); err != nil {
			t.Error(err)
		}
	})
	readCommand, writeCommand, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readResponse, writeResponse, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/usr/bin/google-chrome", "--headless", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-gpu", "--remote-debugging-pipe", "--user-data-dir="+profile, "about:blank")
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=zh_CN.UTF-8", "HOME=" + filepath.Dir(profile), "XDG_RUNTIME_DIR=" + filepath.Dir(profile)}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: computer.BrowserUID, Gid: computer.DesktopSharedGID}, Setpgid: true}
	command.ExtraFiles = []*os.File{readCommand, writeResponse}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(readCommand.Close(), writeResponse.Close()); err != nil {
		t.Fatal(err)
	}
	instance := &liveBrowser{process: command, done: make(chan error, 1)}
	go func() { instance.done <- command.Wait(); close(instance.done) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if instance.service != nil {
			if err := instance.service.Close(ctx); err != nil {
				t.Error(err)
			}
		}
		select {
		case err := <-instance.done:
			if err != nil {
				var exited *exec.ExitError
				if !errors.As(err, &exited) {
					t.Error(err)
				}
			}
		case <-ctx.Done():
			t.Error("浏览器未在关闭管道后退出")
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Error(err)
			}
		}
	})
	service, err := New(context.Background(), readResponse, writeCommand, options)
	if err != nil {
		t.Fatal("真实 Chrome 管道初始化失败", err)
	}
	instance.service = service
	return instance
}

func evaluate(t *testing.T, service *Service, address Address, expression string) string {
	t.Helper()
	service.mu.Lock()
	item := service.windows[address.WindowID]
	service.mu.Unlock()
	if item == nil {
		t.Fatal("测试窗口不存在")
	}
	return evaluateTarget(t, chromedp.FromContext(item.ctx).Target, expression)
}

func evaluateTarget(t *testing.T, target cdp.Session, expression string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	positive := true
	result, err := cdp.Call(ctx, target, runtime.Evaluate, runtime.EvaluateParams{Expression: expression, ReturnByValue: &positive, AwaitPromise: &positive})
	if err != nil || result.ExceptionDetails != nil {
		t.Fatal("测试网页表达式失败", err)
	}
	var value string
	if err := json.Unmarshal(result.Result.Value, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func waitValue(t *testing.T, service *Service, address Address, expression, expected string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if evaluate(t, service, address, expression) == expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("真实页面未出现预期状态")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSharedChromeWindows(t *testing.T) {
	options, access, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "shared-browser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chown(directory, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(directory, "profile")
	var slow atomic.Int32
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/keyboard":
			if _, err := io.WriteString(w, `<!doctype html><meta charset="utf-8"><input id="entry" style="position:absolute;left:10px;top:10px;width:300px;height:40px"><button onclick="document.body.dataset.clicks=String(Number(document.body.dataset.clicks||0)+1)">确认</button>`); err != nil {
				t.Error(err)
			}
			return
		case "/state":
			value := "guest"
			if cookie, err := r.Cookie("fixture_account"); err == nil {
				value = cookie.Value
			}
			if _, err := io.WriteString(w, value); err != nil {
				t.Error(err)
			}
			return
		case "/login":
			value := r.URL.Query().Get("account")
			if value != "first" && value != "second" {
				w.WriteHeader(400)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "fixture_account", Value: value, HttpOnly: true, Path: "/", MaxAge: 3600})
			w.WriteHeader(204)
			return
		case "/logout":
			http.SetCookie(w, &http.Cookie{Name: "fixture_account", Path: "/", MaxAge: -1})
			w.WriteHeader(204)
			return
		case "/slow":
			slow.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		owner := r.URL.Query().Get("owner")
		if owner != "A" {
			owner = "B"
		}
		color := "#dcefff"
		if owner == "B" {
			color = "#ffe8cd"
		}
		_, err := fmt.Fprintf(w, `<!doctype html><html><meta charset="utf-8"><title>机器人 %s 窗口</title><style>body{background:%s;font:24px sans-serif}input{position:absolute;left:20px;top:100px;width:700px;height:45px;font:24px sans-serif}button{position:absolute;left:20px;top:190px;width:240px;height:50px}</style><h1>机器人 %s 独立窗口</h1><input id="entry"><button onclick="window.open('/popup')">打开所属弹窗</button><button style="left:280px" onclick="document.body.dataset.answer=String(prompt('所属窗口的输入框','默认值'))">打开输入框</button><button style="left:540px" onclick="document.body.dataset.answer=String(confirm('确认当前操作'))">打开确认框</button></html>`, owner, color, owner)
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	live := launchTestBrowser(t, profile, options)
	service := live.service
	addresses := []Address{{Session: sessions[0], WindowID: ids.New()}, {Session: sessions[1], WindowID: ids.New()}}
	for index, address := range addresses {
		window, err := service.Open(context.Background(), address)
		if err != nil || window.ID != address.WindowID {
			t.Fatal("创建独立窗口失败", err)
		}
		again, err := service.Open(context.Background(), address)
		if err != nil || again.ID != window.ID {
			t.Fatal("重复开户未复用窗口", err)
		}
		if _, err := service.Execute(context.Background(), address, Command{Sequence: 1, Kind: "navigate", URL: server.URL + "/?owner=" + string(rune('A'+index))}); err != nil {
			t.Fatal(err)
		}
		waitValue(t, service, address, `document.readyState`, "complete")
		deadline := time.Now().Add(options.CallTimeout)
		for {
			windows, err := service.List(t.Context(), address.Session)
			if err != nil {
				t.Fatal(err)
			}
			if len(windows) == 1 && windows[0].Title == "机器人 "+string(rune('A'+index))+" 窗口" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("独立窗口标题未更新", windows)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Run("Chrome 原生键盘事件与回执", func(t *testing.T) {
		address := Address{Session: sessions[0], WindowID: ids.New()}
		if _, err := service.Open(t.Context(), address); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Execute(t.Context(), address, Command{Sequence: 1, Kind: "navigate", URL: server.URL + "/keyboard"}); err != nil {
			t.Fatal(err)
		}
		waitValue(t, service, address, `document.readyState`, "complete")
		commands := []Command{{Sequence: 2, Kind: "click", X: 40, Y: 30}, {Sequence: 3, Kind: "type", Text: "第一次中文"},
			{Sequence: 4, Kind: "key", Key: "A", Modifiers: 2}, {Sequence: 5, Kind: "type", Text: "替换后的中文"},
			{Sequence: 6, Kind: "key", Key: "Tab"}, {Sequence: 7, Kind: "key", Key: "Enter"}}
		for _, command := range commands {
			if _, err := service.Execute(t.Context(), address, command); err != nil {
				t.Fatal(err)
			}
		}
		// 同一回执不能再次激活按钮；通过真实页面结果覆盖 Enter 的字符事件回归。
		if _, err := service.Execute(t.Context(), address, commands[len(commands)-1]); err != nil {
			t.Fatal(err)
		}
		waitValue(t, service, address, `document.querySelector('#entry').value`, "替换后的中文")
		waitValue(t, service, address, `document.body.dataset.clicks`, "1")
		if err := service.CloseWindow(t.Context(), address); err != nil {
			t.Fatal(err)
		}
	})
	var pending sync.WaitGroup
	for index, address := range addresses {
		pending.Go(func() {
			if _, err := service.Execute(context.Background(), address, Command{Sequence: 3, Kind: "click", X: 50, Y: 120}); err != nil {
				t.Error(err)
				return
			}
			command := Command{Sequence: 5, Kind: "type", Text: []string{"甲的中文输入", "乙的中文输入"}[index]}
			for range 2 {
				if _, err := service.Execute(context.Background(), address, command); err != nil {
					t.Error(err)
				}
			}
		})
	}
	pending.Wait()
	for index, address := range addresses {
		waitValue(t, service, address, `document.querySelector('#entry').value`, []string{"甲的中文输入", "乙的中文输入"}[index])
	}
	images := make([][]byte, 2)
	for index, address := range addresses {
		images[index], err = service.Screenshot(context.Background(), address)
		if err != nil {
			t.Fatal(err)
		}
		if report := os.Getenv("TOM_SHARED_BROWSER_REPORT"); report != "" {
			if err := os.WriteFile(filepath.Join(report, fmt.Sprintf("browser-%d.png", index+1)), images[index], 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if bytes.Equal(images[0], images[1]) {
		t.Fatal("两个窗口返回同一画面")
	}
	foreign := addresses[0]
	foreign.Session = sessions[1]
	if _, err := service.Screenshot(context.Background(), foreign); !errors.Is(err, ErrDenied) {
		t.Fatal("跨 Bot 截图被允许", err)
	}
	if _, err := service.Execute(context.Background(), foreign, Command{Sequence: 6, Kind: "type", Text: "wrong"}); !errors.Is(err, ErrDenied) {
		t.Fatal("跨 Bot 输入被允许", err)
	}
	if _, err := service.Execute(context.Background(), addresses[0], Command{Sequence: 5, Kind: "type", Text: "changed"}); !errors.Is(err, ErrConflict) {
		t.Fatal("更换重复操作内容仍成功", err)
	}
	for index, x := range []int{320, 580} {
		command := Command{Sequence: int64(6 + index), Kind: "click", X: x, Y: 210}
		finished := make(chan error, 1)
		receipts := make(chan Window, 1)
		go func() {
			receipt, err := service.Execute(context.Background(), addresses[0], command)
			receipts <- receipt
			finished <- err
		}()
		dialog := waitDialog(t, service, addresses[0])
		// 顺序 Agent 必须先取得动作回执，才能在下一步读取并答复对话框。
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal("触发对话框的动作未确认", err)
			}
		case <-time.After(time.Second):
			t.Fatal("未答复对话框时无法取得页面动作回执")
		}
		receipt := <-receipts
		if receipt.CommandState != "waiting_dialog" || receipt.Dialog.ID != dialog.ID || receipt.Sequence != command.Sequence {
			t.Fatal("等待回执未标明原动作及对话框")
		}
		if duplicate, err := service.Execute(t.Context(), addresses[0], command); err != nil || duplicate != receipt {
			t.Fatal("在途动作没有复用等待回执", err)
		}
		if _, err := service.Execute(t.Context(), addresses[0], Command{Sequence: command.Sequence + 1, Kind: "reload"}); !errors.Is(err, ErrDialog) {
			t.Fatal("等待对话框时接受了下一动作", err)
		}
		if _, err := service.Screenshot(t.Context(), addresses[0]); !errors.Is(err, ErrDialog) {
			t.Fatal("等待对话框时未明确返回截图不可用", err)
		}
		reply := DialogReply{ID: dialog.ID}
		expected := "false"
		if index == 0 {
			if dialog.Kind != "prompt" || dialog.DefaultPrompt != "默认值" {
				t.Fatal("输入框元数据错误")
			}
			reply.Accept, reply.Text, expected = true, "中文答复", "中文答复"
		}
		if _, err := service.Screenshot(context.Background(), addresses[1]); err != nil {
			t.Fatal("对话框阻塞其他 Bot", err)
		}
		if err := service.RespondDialog(context.Background(), foreign, reply); !errors.Is(err, ErrDenied) {
			t.Fatal("跨 Bot 可以答复对话框", err)
		}
		wrong := reply
		wrong.ID++
		if err := service.RespondDialog(context.Background(), addresses[0], wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("错误对话框序号通过", err)
		}
		for range 2 {
			if err := service.RespondDialog(context.Background(), addresses[0], reply); err != nil {
				t.Fatal("对话框答复或回执复用失败", err)
			}
		}
		waitValue(t, service, addresses[0], `document.body.dataset.answer`, expected)
		waitCommandState(t, service, addresses[0], command.Sequence, "completed")
		wrong = reply
		wrong.Accept = !reply.Accept
		wrong.Text = ""
		if err := service.RespondDialog(context.Background(), addresses[0], wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("重复对话框答复内容被替换", err)
		}
	}
	state := `fetch('/state').then(r=>r.text())`
	if got := evaluate(t, service, addresses[0], `fetch('/login?account=first').then(r=>String(r.status))`); got != "204" {
		t.Fatal("登录测试状态错误")
	}
	waitValue(t, service, addresses[1], state, "first")
	evaluate(t, service, addresses[0], `localStorage.setItem('shared','共享状态'); 'saved'`)
	waitValue(t, service, addresses[1], `localStorage.getItem('shared')`, "共享状态")
	const saveDatabase = `new Promise((resolve,reject)=>{const r=indexedDB.open('shared_fixture',1);r.onupgradeneeded=()=>r.result.createObjectStore('state');r.onerror=()=>reject(r.error);r.onsuccess=()=>{const d=r.result,t=d.transaction('state','readwrite');t.objectStore('state').put('共享数据库','value');t.oncomplete=()=>{d.close();resolve('saved')};t.onerror=()=>reject(t.error)}})`
	const readDatabase = `new Promise((resolve,reject)=>{const r=indexedDB.open('shared_fixture',1);r.onerror=()=>reject(r.error);r.onsuccess=()=>{const d=r.result,t=d.transaction('state'),q=t.objectStore('state').get('value');q.onsuccess=()=>resolve(q.result);q.onerror=()=>reject(q.error);t.oncomplete=()=>d.close()}})`
	if evaluate(t, service, addresses[0], saveDatabase) != "saved" {
		t.Fatal("共享数据库写入失败")
	}
	waitValue(t, service, addresses[1], readDatabase, "共享数据库")
	evaluate(t, service, addresses[1], `fetch('/login?account=second').then(r=>String(r.status))`)
	waitValue(t, service, addresses[0], state, "second")
	evaluate(t, service, addresses[0], `fetch('/logout').then(r=>String(r.status))`)
	waitValue(t, service, addresses[1], state, "guest")
	if _, err := service.Execute(context.Background(), addresses[0], Command{Sequence: 8, Kind: "click", X: 100, Y: 210}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		windows, err := service.List(context.Background(), sessions[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(windows) == 2 {
			if windows[0].ParentID != "" && windows[0].ParentID != addresses[0].WindowID || windows[1].ParentID != "" && windows[1].ParentID != addresses[0].WindowID {
				t.Fatal("弹窗归属错误")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("弹窗未归属于创建 Bot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	other, err := service.List(context.Background(), sessions[1])
	if err != nil || len(other) != 1 {
		t.Fatal("弹窗泄漏到另一 Bot", err)
	}
	// 页面自行关闭与显式关闭可以竞争；已关闭窗口的创建回执不能复活页面。
	closing := Address{Session: sessions[0], WindowID: ids.New()}
	if _, err := service.Open(context.Background(), closing); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	closingTarget := service.windows[closing.WindowID].target
	service.mu.Unlock()
	closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := cdp.Call(closeContext, service.browser, target.CloseTarget, target.CloseTargetParams{TargetID: closingTarget}); err != nil {
		t.Fatal(err)
	}
	closeCancel()
	for range 2 {
		if err := service.CloseWindow(context.Background(), closing); err != nil {
			t.Fatal("重复关闭没有确认页面消失", err)
		}
	}
	if _, err := service.Open(context.Background(), closing); !errors.Is(err, ErrConflict) {
		t.Fatal("已关闭窗口被迟到请求重新打开", err)
	}
	// 一个页面的未知导航不能锁住另一 Bot；已发出的导航不能重放。
	slowAddress := Address{Session: sessions[0], WindowID: ids.New()}
	if _, err := service.Open(context.Background(), slowAddress); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := service.Execute(bounded, slowAddress, Command{Sequence: 1, Kind: "navigate", URL: server.URL + "/slow"})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("慢导航未开始")
	}
	parallel, stopParallel := context.WithTimeout(context.Background(), time.Second)
	_, err = service.Screenshot(parallel, addresses[1])
	stopParallel()
	if err != nil || bounded.Err() != nil {
		t.Fatal("慢导航阻塞其他 Bot 的窗口", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, ErrUnknown) {
		t.Fatal("未确认导航没有保留未知", err)
	}
	for _, command := range []Command{{Sequence: 1, Kind: "navigate", URL: server.URL + "/slow"}, {Sequence: 2, Kind: "reload"}} {
		if _, err := service.Execute(context.Background(), slowAddress, command); !errors.Is(err, ErrUnknown) || slow.Load() != 1 {
			t.Fatal("未知动作后继续发送", err)
		}
	}
	access.revoke(sessions[0])
	if err := service.ReleaseSession(context.Background(), sessions[0]); err != nil {
		t.Fatal("定向关闭失败", err)
	}
	if _, err := service.Screenshot(context.Background(), addresses[1]); err != nil {
		t.Fatal("关闭其他 Bot 影响本窗口", err)
	}
	evaluate(t, service, addresses[1], `fetch('/login?account=first').then(r=>String(r.status))`)
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-live.done:
		if err != nil {
			t.Fatal("浏览器退出失败", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("共享浏览器退出超时")
	}
	// 使用原 profile 再启动真实 Chrome，证明登录态不是内存复制或临时 Context。
	live = launchTestBrowser(t, profile, options)
	service = live.service
	address := Address{Session: sessions[1], WindowID: ids.New()}
	if _, err := service.Open(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), address, Command{Sequence: 1, Kind: "navigate", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	waitValue(t, service, address, state, "first")
	waitValue(t, service, address, `localStorage.getItem('shared')`, "共享状态")
	waitValue(t, service, address, readDatabase, "共享数据库")
	// 使用私有诊断页面核对实际沙箱；公开窗口接口不开放 chrome:// 导航。
	sandboxContext, stopSandbox := context.WithTimeout(context.Background(), 3*time.Second)
	rootTarget := chromedp.FromContext(service.root).Target
	if _, err := cdp.Call(sandboxContext, rootTarget, page.Navigate, page.NavigateParams{URL: "chrome://sandbox"}); err != nil {
		t.Fatal("沙箱诊断页不可用", err)
	}
	stopSandbox()
	status := ""
	deadline = time.Now().Add(3 * time.Second)
	for !strings.Contains(status, "sandboxed.") {
		status = evaluateTarget(t, rootTarget, `document.body ? document.body.innerText : ''`)
		if time.Now().After(deadline) {
			t.Fatal("Chrome 沙箱状态没有完成加载")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if report := os.Getenv("TOM_SHARED_BROWSER_REPORT"); report != "" {
		if err := os.WriteFile(filepath.Join(report, "chrome-sandbox.txt"), []byte(status), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, feature := range []string{"PID namespaces", "Network namespaces", "Seccomp-BPF sandbox"} {
		found := false
		for line := range strings.SplitSeq(status, "\n") {
			if strings.Contains(line, feature) && strings.Contains(line, "Yes") {
				found = true
			}
		}
		if !found {
			t.Fatal("Chrome 沙箱缺少能力", feature)
		}
	}
	// 全局 CDP 地址只存在于私有管道，启动命令不得开放网络调试端口或关闭沙箱。
	for _, argument := range live.process.Args {
		if strings.HasPrefix(argument, "--remote-debugging-port") || argument == "--no-sandbox" {
			t.Fatal("浏览器调试入口或沙箱配置错误")
		}
	}
}

func waitCommandState(t *testing.T, service *Service, address Address, sequence int64, state string) {
	t.Helper()
	deadline := time.Now().Add(service.options.CallTimeout + service.options.DialogTimeout + time.Second)
	for {
		windows, err := service.List(t.Context(), address.Session)
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range windows {
			if window.ID == address.WindowID && window.Sequence == sequence && window.CommandState == state {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("页面动作未进入预期状态", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDialog(t *testing.T, service *Service, address Address) Dialog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		windows, err := service.List(context.Background(), address.Session)
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range windows {
			if window.ID == address.WindowID && window.Dialog.ID != 0 {
				return window.Dialog
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("所属页面没有出现对话框事件")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSharedChromeFailureRevokesAllWindows(t *testing.T) {
	options, _, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "shared-browser-failure-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chown(directory, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	live := launchTestBrowser(t, filepath.Join(directory, "profile"), options)
	var addresses []Address
	for _, session := range sessions {
		address := Address{Session: session, WindowID: ids.New()}
		addresses = append(addresses, address)
		if _, err := live.service.Open(context.Background(), address); err != nil {
			t.Fatal(err)
		}
	}
	if err := live.process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.service.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("共享浏览器失联后没有结束服务")
	}
	for _, address := range addresses {
		if _, err := live.service.List(context.Background(), address.Session); !errors.Is(err, ErrUnavailable) {
			t.Fatal("共享故障后仍可枚举窗口", err)
		}
		if _, err := live.service.Screenshot(context.Background(), address); !errors.Is(err, ErrUnavailable) {
			t.Fatal("共享故障后仍可获取画面", err)
		}
		if _, err := live.service.Execute(context.Background(), address, Command{Sequence: 1, Kind: "reload"}); !errors.Is(err, ErrUnavailable) {
			t.Fatal("共享故障后仍可发送动作", err)
		}
	}
}
