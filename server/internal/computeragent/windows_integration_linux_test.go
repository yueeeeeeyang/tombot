//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

// windowRecords 仅替代 Chrome 页面状态；入口、HTTP、内核 UID 与回收流程使用实际实现。
type windowRecords struct {
	unusedWindowBrowser
	mu           sync.Mutex
	pages        map[computer.ExecutionSession][]sharedbrowser.Window
	released     []computer.ExecutionSession
	releaseError error
	release      func(context.Context) error
	list         func(context.Context) error
}

func (records *windowRecords) Open(ctx context.Context, address sharedbrowser.Address) (sharedbrowser.Window, error) {
	records.mu.Lock()
	defer records.mu.Unlock()
	window := sharedbrowser.Window{ID: address.WindowID, State: "ready"}
	records.pages[address.Session] = append(records.pages[address.Session], window)
	return window, ctx.Err()
}

func (records *windowRecords) List(ctx context.Context, session computer.ExecutionSession) ([]sharedbrowser.Window, error) {
	if records.list != nil {
		if err := records.list(ctx); err != nil {
			return nil, err
		}
	}
	records.mu.Lock()
	defer records.mu.Unlock()
	return append([]sharedbrowser.Window{}, records.pages[session]...), ctx.Err()
}

func (records *windowRecords) ReleaseSession(ctx context.Context, session computer.ExecutionSession) error {
	if records.release != nil {
		if err := records.release(ctx); err != nil {
			return err
		}
	}
	records.mu.Lock()
	defer records.mu.Unlock()
	records.released = append(records.released, session)
	if records.releaseError != nil {
		return records.releaseError
	}
	delete(records.pages, session)
	return ctx.Err()
}

type windowFixture struct {
	fixture
	browsers   *BrowserController
	records    *windowRecords
	references []processgroup.Reference
	fatals     atomic.Int32
}

func newWindowFixture(t *testing.T) *windowFixture {
	t.Helper()
	requireBrowserContainer(t)
	if err := prepareFilesystem(t.Context(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	f := &windowFixture{fixture: newFixture(t), records: &windowRecords{pages: map[computer.ExecutionSession][]sharedbrowser.Window{}}}
	f.command.Lifetime = 30 * time.Second
	second, reference := desktopCommand(f.command, 2)
	f.references = []processgroup.Reference{f.reference, reference}
	for index, command := range []computer.SessionCommand{f.command, second} {
		if _, err := f.controller.Open(t.Context(), command, f.references[index], f.files(t)); err != nil {
			t.Fatal(err)
		}
		directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, command.Session)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	controller, _, factory := newBrowserControlFixture(t, f.fixture)
	f.browsers = controller
	factory.prepare = func(scope *fakeBrowserScope) {
		scope.start = func(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error) {
			return f.records, nil
		}
	}
	if _, err := controller.startAt(t.Context(), browserGrant(f.fixture), probeFiles(t), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *windowFixture) open(t *testing.T, index int, check func(context.Context, processgroup.Reference, sharedbrowser.Peer) error) *windowSession {
	t.Helper()
	windows, err := openWindowSessionWithPeer(f.references[index], f.browsers, func(ctx context.Context, session computer.ExecutionSession) error {
		return f.controller.withScope(ctx, session, func(context.Context, *desktopGuard) error { return nil })
	}, f.controller.options, f.guard.logger, func() { f.fatals.Add(1) }, check)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := windows.Close(ctx); err != nil && f.fatals.Load() == 0 {
			t.Error(err)
		}
	})
	return windows
}

// 容器没有可写 cgroup；该替身仍读取真实对端，仅放行已确认的容器根组，不作为生产入组证据。
func testWindowPeer(ctx context.Context, reference processgroup.Reference, peer sharedbrowser.Peer) error {
	encoded, err := os.ReadFile("/proc/" + strconv.Itoa(peer.PID) + "/cgroup")
	if err != nil || strings.TrimSpace(string(encoded)) != "0::/" || peer.UID != uint32(reference.Session.UID) || peer.GID != uint32(reference.Session.GID) {
		return sharedbrowser.ErrDenied
	}
	return ctx.Err()
}

func requestWindow(ctx context.Context, session computer.ExecutionSession, uid int, method, endpoint, body string) (int, []byte, error) {
	program := `import http.client,json,socket,sys
class Client(http.client.HTTPConnection):
 def connect(self):
  self.sock=socket.socket(socket.AF_UNIX)
  self.sock.settimeout(3)
  self.sock.connect(sys.argv[1])
c=Client("browser.local",timeout=3)
c.request(sys.argv[2],sys.argv[3],body=sys.argv[4] or None,headers={"Content-Type":"application/json"})
r=c.getresponse()
b=r.read(65537)
if len(b)>65536: raise RuntimeError("response too large")
print(json.dumps({"status":r.status,"body":b.decode()}))
c.close()`
	command := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-B", "-c", program, filepath.Join(session.RuntimeDirectory(), windowSocketName), method, endpoint, body)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(session.GID)}}
	encoded, err := command.Output()
	if err != nil {
		return 0, nil, err
	}
	var response struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		return 0, nil, err
	}
	return response.Status, []byte(response.Body), nil
}

// liveWindowGateways 将实际 Chrome 与正式窗口入口组合，内核进程入组仍由明确的受限容器夹具替代。
func liveWindowGateways(t *testing.T, browsers *BrowserController, grant BrowserGrant, options sharedbrowser.Options, sessions []computer.ExecutionSession) []*windowSession {
	t.Helper()
	if err := prepareFilesystem(t.Context(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	var result []*windowSession
	for _, session := range sessions {
		directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, session)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
		parent := grant.Request.Group.Container
		reference := processgroup.Reference{Session: session, ParentPath: parent.Path, ParentDevice: parent.Device, ParentInode: parent.Inode}
		entry, err := openWindowSessionWithPeer(reference, browsers, options.Authorize, browsers.options, options.Logger, func() { t.Error("实际窗口入口失控") }, testWindowPeer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := entry.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		result = append(result, entry)
	}
	return result
}

func TestContainerWindowSessionOwnershipAndRetirement(t *testing.T) {
	f := newWindowFixture(t)
	first, second := f.open(t, 0, testWindowPeer), f.open(t, 1, testWindowPeer)
	for _, windows := range []*windowSession{first, second} {
		status, _, err := requestWindow(t.Context(), windows.session, windows.session.UID, "PUT", "/v2/windows/"+ids.New(), "{}")
		if err != nil || status != http.StatusOK {
			t.Fatal("所属桌面无法创建窗口", status, err)
		}
	}
	status, encoded, err := requestWindow(t.Context(), first.session, first.session.UID, "GET", "/v2/state", "")
	var state struct {
		Windows []sharedbrowser.Window `json:"windows"`
	}
	if err != nil || status != 200 || json.Unmarshal(encoded, &state) != nil || len(state.Windows) != 1 {
		t.Fatal("窗口列表未按桌面过滤", status, err)
	}
	for _, uid := range []int{0, second.session.UID} {
		if _, _, err := requestWindow(t.Context(), first.session, uid, "GET", "/v2/state", ""); err == nil {
			t.Fatal("其他内核身份访问了窗口入口")
		}
	}
	if duplicate, err := openWindowSession(f.references[0], f.browsers, func(context.Context, computer.ExecutionSession) error { return nil }, f.controller.options, f.guard.logger, func() { f.fatals.Add(1) }); err == nil || duplicate != nil {
		t.Fatal("重复入口覆盖已有监听")
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal("重复关闭失败", err)
	}
	if _, err := os.Lstat(filepath.Join(first.session.RuntimeDirectory(), windowSocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("关闭后遗留入口", err)
	}
	status, _, err = requestWindow(t.Context(), second.session, second.session.UID, "GET", "/v2/state", "")
	if status != 200 || err != nil {
		t.Fatal("回收甲影响了乙", status, err)
	}
	f.records.mu.Lock()
	defer f.records.mu.Unlock()
	if len(f.records.released) != 1 || f.records.released[0] != first.session || len(f.records.pages[second.session]) != 1 || f.fatals.Load() != 0 {
		t.Fatal("页面回收越界或重复", f.records.released)
	}
}

func TestContainerWindowSessionRejectsUnownedGroup(t *testing.T) {
	f := newWindowFixture(t)
	windows := f.open(t, 0, checkWindowPeer)
	status, _, err := requestWindow(t.Context(), windows.session, windows.session.UID, "GET", "/v2/state", "")
	if err != nil || status != 403 {
		t.Fatal("同 UID 但错误资源组未拒绝", status, err)
	}
}

func TestContainerWindowSessionCancelsBeforePageRelease(t *testing.T) {
	f := newWindowFixture(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	f.records.list = func(ctx context.Context) error { close(entered); <-ctx.Done(); close(exited); return ctx.Err() }
	windows := f.open(t, 0, testWindowPeer)
	done := make(chan error, 1)
	go func() {
		_, _, err := requestWindow(t.Context(), windows.session, windows.session.UID, "GET", "/v2/state", "")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("窗口请求没有进入")
	}
	windows.Revoke()
	select {
	case <-windows.retired:
	case <-time.After(2 * time.Second):
		t.Fatal("撤销没有独立回收页面")
	}
	select {
	case <-exited:
	default:
		t.Fatal("页面回收前未取消在途请求")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端连接未结束")
	}
	if len(f.records.released) != 1 || f.fatals.Load() != 0 {
		t.Fatal("撤销没有完成所属页面回收")
	}
	if _, err := os.Lstat(filepath.Join(windows.session.RuntimeDirectory(), windowSocketName)); err != nil {
		t.Fatal("任务排空前提前移除了入口证据", err)
	}
	if err := windows.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestContainerWindowSessionRetirementWatchdog(t *testing.T) {
	f := newWindowFixture(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.records.release = func(context.Context) error {
		close(entered)
		<-release
		defer close(finished)
		return errors.New("测试回收阻塞")
	}
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("回收测试协程未结束")
		}
	})
	windows := f.open(t, 0, testWindowPeer)
	windows.Revoke()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("未独立发起页面回收")
	}
	select {
	case <-windows.retired:
	case <-time.After(2 * time.Second):
		t.Fatal("页面回收阻塞未被看门狗发现")
	}
	if f.fatals.Load() != 1 || !errors.Is(windows.retireError, context.DeadlineExceeded) {
		t.Fatal("阻塞未升级容器故障", windows.retireError)
	}
	if windows.Close(t.Context()) == nil {
		t.Fatal("未知回收被确认关闭")
	}
}

func TestContainerWindowSessionFailurePreservesBarrier(t *testing.T) {
	for _, scenario := range []string{"release", "replacement", "instance"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWindowFixture(t)
			windows := f.open(t, 0, testWindowPeer)
			path := filepath.Join(windows.session.RuntimeDirectory(), windowSocketName)
			switch scenario {
			case "release":
				f.records.releaseError = errors.New("页面回收未知")
			case "replacement":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("保留替换内容"), 0600); err != nil {
					t.Fatal(err)
				}
			case "instance":
				f.browsers.available.Store(nil)
				status, _, err := requestWindow(t.Context(), windows.session, windows.session.UID, "GET", "/v2/state", "")
				if status != 403 || err != nil {
					t.Fatal("旧入口没有绑定原共享实例", status, err)
				}
			}
			err := windows.Close(t.Context())
			if scenario == "instance" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || f.fatals.Load() != 1 {
				t.Fatal("清理失败没有封闭容器", err)
			}
			if windows.Close(t.Context()) == nil || len(f.records.released) != 1 || f.fatals.Load() != 1 {
				t.Fatal("未知页面关闭被重发或错误被忽略")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("失败后删除了入口证据", err)
			}
			if scenario == "replacement" {
				encoded, err := os.ReadFile(path)
				if err != nil || string(encoded) != "保留替换内容" {
					t.Fatal("误删替换文件", err)
				}
			}
		})
	}
}
