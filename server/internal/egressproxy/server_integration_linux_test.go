//go:build linux && egressintegration

package egressproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func kernelListener(t *testing.T) *net.UnixListener {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("TOM_EGRESS_TEST") != "1" {
		t.Fatal("出网内核验收必须在专属 root 受限容器运行")
	}
	directory, err := os.MkdirTemp("/run", "egress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "egress.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return listener
}

func TestKernelEgressIdentityAndShutdown(t *testing.T) {
	listener := kernelListener(t)
	proxy := testProxy(t)
	var peers, dials atomic.Int32
	proxy.check = func(ctx context.Context, peer Peer) error {
		peers.Add(1)
		if peer.PID != int32(os.Getpid()) {
			return errors.New("测试拒绝其他进程")
		}
		return ctx.Err()
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	pointAt(proxy, upstream.Listener.Addr().String(), &dials)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() { finished <- proxy.Serve(ctx, listener); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("内核出网服务未退出")
		}
	})
	proxyURL, err := url.Parse("http://local.invalid")
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("http://web.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	readResponse(t, response)
	if response.StatusCode != 204 || peers.Load() != 1 || dials.Load() != 1 {
		t.Fatal("真实 Unix 身份或请求转发失败")
	}
	for _, uid := range []uint32{0, 20001} {
		// 临时放宽测试端点访问，仅用于确认内核身份检查自身仍拒绝错误进程和非 root。
		if err := os.Chmod(filepath.Dir(listener.Addr().String()), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(listener.Addr().String(), 0666); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=^TestKernelEgressChild$", "-test.timeout=5s")
		command.Env = append(os.Environ(), "TOM_EGRESS_CHILD="+listener.Addr().String())
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("错误身份拒绝测试失败: %v %s", err, output)
		}
		if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if peers.Load() != 2 || dials.Load() != 1 {
		t.Fatal("非 root 到达容器授权器或错误身份发起出网")
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: listener.Addr().String(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if RequireHostConnection(connection) == nil {
		t.Fatal("同 PID 空间进程冒充宿主")
	}
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cancel()
	var one [1]byte
	if _, err := connection.Read(one[:]); err == nil {
		t.Fatal("取消未关闭空闲连接")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("内核代理正常取消失败", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("内核代理取消超时")
	}
	proxy.mu.Lock()
	remaining := len(proxy.connections)
	proxy.mu.Unlock()
	if remaining != 0 || len(proxy.slots) != 0 {
		t.Fatal("内核代理退出遗留连接")
	}
}

func TestKernelEgressChild(t *testing.T) {
	path := os.Getenv("TOM_EGRESS_CHILD")
	if path == "" {
		return
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal("测试连接未到达内核校验", err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, "GET http://web.example.test/ HTTP/1.1\r\nHost: web.example.test\r\n\r\n"); err != nil {
		return
	}
	var one [1]byte
	if _, err := connection.Read(one[:]); err == nil {
		t.Fatal("错误身份收到代理响应")
	}
}

func TestKernelEgressProtectedEndpoint(t *testing.T) {
	listener := kernelListener(t)
	proxy := testProxy(t)
	if err := os.Chmod(listener.Addr().String(), 0666); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Serve(t.Context(), listener); err == nil {
		t.Fatal("接受非私有监听端点")
	}
}

func TestKernelOwnedEndpointRecoveryAndReplacement(t *testing.T) {
	old := kernelListener(t)
	path := old.Addr().String()
	if listener, err := Listen(path); err == nil {
		t.Error("活端点被接管")
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}
	old.SetUnlinkOnClose(false)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(path)
	if err != nil {
		t.Fatal("不能清理已停止的遗留端点", err)
	}
	if err := validateListener(listener.UnixListener); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Listen(path); err == nil {
		t.Error("稳定锁允许第二个监听者")
		if err := duplicate.Close(); err != nil {
			t.Error(err)
		}
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("退出未删除原 socket", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("稳定锁被删除", err)
	}
	listener, err = Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("保留未知文件"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Check(); err == nil {
		t.Fatal("被替换端点仍被标记可用")
	}
	if err := listener.Close(); err == nil {
		t.Fatal("被替换端点未报告错误")
	}
	encoded, err := os.ReadFile(path)
	if err != nil || string(encoded) != "保留未知文件" {
		t.Fatal("删除了被替换的端点", err)
	}
	if replacement, err := Listen(path); err == nil {
		t.Error("普通文件被接管")
		if err := replacement.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestKernelDrainDoesNotConfuseTimeoutWithCompletion(t *testing.T) {
	listener := kernelListener(t)
	proxy := testProxy(t)
	proxy.options.ShutdownTimeout = 20 * time.Millisecond
	// 模拟仍未退出的处理者；Serve 必须报告超时，Drain 不能因此报告已排空。
	proxy.pending.Add(1)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { finished <- proxy.Serve(ctx, listener) }()
	until := time.Now().Add(time.Second)
	for !proxy.started.Load() {
		if time.Now().After(until) {
			t.Fatal("代理未启动")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("未报告工作者退出超时")
	}
	short, stop := context.WithTimeout(t.Context(), 10*time.Millisecond)
	if err := proxy.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("未结束工作者被当作已回收", err)
	}
	stop()
	proxy.pending.Done()
	bounded, release := context.WithTimeout(t.Context(), time.Second)
	defer release()
	if err := proxy.Drain(bounded); err != nil {
		t.Fatal(err)
	}
}
