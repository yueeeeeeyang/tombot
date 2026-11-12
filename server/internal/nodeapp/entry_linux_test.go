//go:build linux && nodeintegration && !cgo

package nodeapp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
)

type entryFixture struct {
	configuration Config
	path          string
	reads         atomic.Int32
}

// newEntryFixture 使用真实组件构造器和 Unix HTTP，Docker 仅有“资源不存在”的明确替身；不写宿主 cgroup 或挂载磁盘。
func newEntryFixture(t *testing.T) *entryFixture {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("TOM_NODE_ENTRY_TEST") != "1" {
		t.Fatal("节点入口集成测试要求专用 Linux root 容器")
	}
	directory, err := os.MkdirTemp("/run", "node-entry-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0711); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	c := testConfig()
	c.Socket = filepath.Join(directory, "control.sock")
	c.Runtime.Socket = filepath.Join(directory, "docker.sock")
	c.Runtime.StateDirectory = filepath.Join(directory, "runtime")
	c.Runtime.StorageDirectory = filepath.Join(directory, "workspaces")
	c.Storage.StateDirectory = filepath.Join(directory, "storage")
	for _, path := range []string{c.Runtime.StateDirectory, c.Runtime.StorageDirectory, c.Storage.StateDirectory} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f := &entryFixture{configuration: c, path: filepath.Join(directory, "node.json")}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", c.Runtime.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.Runtime.Socket, 0660); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && (strings.HasPrefix(r.URL.Path, "/v1.47/volumes/") || strings.HasPrefix(r.URL.Path, "/v1.47/containers/")) {
			f.reads.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Error("空节点发生非只读 Docker 操作", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return f
}

func (f *entryFixture) client(t *testing.T) {
	t.Helper()
	user := ids.New()
	specification := computer.Specification{OperationID: ids.New(), UserID: user, ComputerID: ids.New(), NodeID: f.configuration.NodeID, ImageReference: "example.invalid/tombot@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + user, Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 2 << 30, ProcessLimit: 64}}
	encoded, err := json.Marshal(specification)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLinuxEntryClientHelper$")
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_ENTRY_CLIENT=" + string(encoded), "TOM_ENTRY_SOCKET=" + f.configuration.Socket}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: f.configuration.ClientUID, Gid: f.configuration.ClientGID}}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("真实非 root 客户端失败：%v %s", err, output)
	}
}

func TestLinuxEntryClientHelper(t *testing.T) {
	encoded := os.Getenv("TOM_ENTRY_CLIENT")
	if encoded == "" {
		t.Skip("只由父测试以专用身份启动")
	}
	if os.Geteuid() == 0 {
		t.Fatal("客户端仍为 root")
	}
	var specification computer.Specification
	if err := json.Unmarshal([]byte(encoded), &specification); err != nil {
		t.Fatal(err)
	}
	client, err := nodeapi.NewClient(nodeapi.ClientOptions{NodeID: specification.NodeID, Socket: os.Getenv("TOM_ENTRY_SOCKET"), CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := client.Heartbeat(t.Context(), computer.NodeQuery{NodeID: specification.NodeID})
	if err != nil || heartbeat.Validate(computer.NodeQuery{NodeID: specification.NodeID}) != nil {
		t.Fatal("真实控制心跳无效", err)
	}
	observation, err := client.Inspect(t.Context(), specification)
	if err != nil || !observation.Matches(specification) || observation.State != "absent" || observation.Capabilities.Ready() {
		t.Fatal("空节点实际状态错误", observation, err)
	}
}

func TestLinuxEntryStartupAndRestart(t *testing.T) {
	f := newEntryFixture(t)
	configuration, err := LoadConfig(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		application, err := New(t.Context(), configuration, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- application.Serve(ctx, nil) }()
		f.client(t)
		duplicate, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
		other, err := New(duplicate, configuration, testLogger())
		stop()
		if err == nil {
			cancel()
			if closeErr := other.Close(t.Context()); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("重复入口取得节点控制权")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("节点未有界退出")
		}
		if _, err := os.Lstat(configuration.Socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("控制端点未释放", err)
		}
	}
	if f.reads.Load() < 4 {
		t.Fatal("没有真正向 Docker 协议对账")
	}
}

func TestLinuxEntryRejectsUnsafeConfigurationAndRecovery(t *testing.T) {
	f := newEntryFixture(t)
	if err := os.Chmod(f.path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(f.path); err == nil {
		t.Fatal("公开配置被接受")
	}
	if err := os.Chmod(f.path, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.configuration.Runtime.StateDirectory, ids.New()+".probe.json")
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), f.configuration, testLogger()); err == nil {
		t.Fatal("损坏恢复记录未阻止启动")
	}
	if _, err := os.Lstat(f.configuration.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("恢复前开放了控制入口", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("失败删除了恢复证据", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), f.configuration, testLogger())
	if err != nil {
		t.Fatal("失败的构造器遗留锁", err)
	}
	if err := application.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxNodeBinaryNotifyAndSignal(t *testing.T) {
	f := newEntryFixture(t)
	executable := os.Getenv("TOM_NODE_BINARY")
	if executable == "" {
		t.Fatal("必须提供本次构建的正式节点入口")
	}
	notifyPath := filepath.Join(filepath.Dir(f.path), "notify.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := notify.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw, err := notify.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var optionErr error
	if err := raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1) }); err != nil || optionErr != nil {
		t.Fatal(err, optionErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_NODE_CONFIG=" + f.path, "NOTIFY_SOCKET=" + notifyPath}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	defer func() {
		cancel()
		if finished {
			return
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("节点测试进程未退出")
		}
	}()
	if err := notify.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 1024)
	control := make([]byte, unix.CmsgSpace(unix.SizeofUcred))
	count, controlCount, flags, _, err := notify.ReadMsgUnix(message, control)
	if err != nil || !strings.Contains(string(message[:count]), "READY=1") {
		t.Fatal("正式进程未确认启动", err)
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		t.Fatal("启动通知或身份被截断")
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlCount])
	if err != nil || len(messages) != 1 {
		t.Fatal("启动通知缺少内核身份", err)
	}
	credentials, err := unix.ParseUnixCredentials(&messages[0])
	if err != nil || credentials.Pid != int32(command.Process.Pid) || credentials.Uid != 0 {
		t.Fatal("通知并非由节点主进程发送", err)
	}
	f.client(t)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("正式进程 SIGTERM 未完成退出")
	}
	if _, err := os.Lstat(f.configuration.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("正式进程遗留控制端点", err)
	}
}

func TestLinuxNodeBinaryRejectsBrowserRecoveryBeforeListening(t *testing.T) {
	f := newEntryFixture(t)
	executable := os.Getenv("TOM_NODE_BINARY")
	if executable == "" {
		t.Fatal("必须提供本次构建的正式节点入口")
	}
	path := filepath.Join(f.configuration.Runtime.StateDirectory, ids.New()+".browser.json")
	encoded := []byte(`{"version":999}`)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{"PATH=/usr/bin:/bin", "TOM_NODE_CONFIG=" + f.path}
	if err := command.Run(); err == nil || ctx.Err() != nil {
		t.Fatal("浏览器恢复失败未及时退出", err)
	}
	if _, err := os.Lstat(f.configuration.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("浏览器恢复前开放监听", err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(encoded) {
		t.Fatal("错误记录未保留", err)
	}
	// 子进程退出必须释放节点锁；删除仅发生在本测试创建的损坏夹具中。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), f.configuration, testLogger())
	if err != nil {
		t.Fatal("失败进程遗留节点锁", err)
	}
	if err := application.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
