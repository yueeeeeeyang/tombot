//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

const gatewayClientScript = `
import base64, http.client, json, socket, sys
class Connection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(6)
        self.sock.connect(sys.argv[1])
connection = Connection('browser.local', timeout=6)
try:
    connection.request(sys.argv[2], sys.argv[3], body=sys.argv[4] or None, headers={'Content-Type':'application/json'})
    response = connection.getresponse()
    encoded = response.read(5*1024*1024)
    print(json.dumps({'status':response.status,'body':base64.b64encode(encoded).decode()}))
finally:
    connection.close()
`

func runGatewayClient(ctx context.Context, path string, uid int, method, endpoint, body string) ([]byte, error) {
	command := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-B", "-c", gatewayClientScript, path, method, endpoint, body)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: computer.DesktopSharedGID}}
	return command.Output()
}

func gatewayClient(t *testing.T, path string, uid int, method, endpoint, body string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	encoded, err := runGatewayClient(ctx, path, uid, method, endpoint, body)
	if err != nil {
		t.Fatal("桌面客户端请求失败", err)
	}
	var response struct {
		Status int
		Body   string
	}
	if json.Unmarshal(encoded, &response) != nil {
		t.Fatal("桌面客户端响应无效")
	}
	decoded, err := base64.StdEncoding.DecodeString(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.Status, decoded
}

func TestSharedChromeDesktopGateway(t *testing.T) {
	options, access, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "browser-gateway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(directory, "browser")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(profile, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	live := launchTestBrowser(t, filepath.Join(profile, "profile"), options)
	server, err := NewGateway(live.service, sessions[0], func(ctx context.Context, session computer.ExecutionSession, peer Peer) error {
		// 测试容器只存在根资源组；这里验证真实对端，生产必须核对完整会话子组。
		encoded, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(peer.PID), "cgroup"))
		if err != nil || strings.TrimSpace(string(encoded)) != "0::/" {
			return ErrDenied
		}
		return access.check(ctx, session)
	}, GatewayOptions{Width: options.Width, Height: options.Height, MaximumConnections: 4, CallTimeout: 6 * time.Second, ShutdownTimeout: time.Second}, options.Logger)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "browser.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 0, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("浏览器桌面入口未结束")
		}
	})
	uid := sessions[0].UID
	if status, _ := gatewayClient(t, path, uid, "GET", "/v2/state", ""); status != 200 {
		t.Fatal("真实所属身份不能读取状态", status)
	}
	base := "/v2/windows/" + ids.New()
	if status, _ := gatewayClient(t, path, uid, "PUT", base, "{}"); status != 200 {
		t.Fatal("客户端不能创建所属页面", status)
	}
	if status, encoded := gatewayClient(t, path, uid, "GET", base+"/image", ""); status != 200 || !bytes.HasPrefix(encoded, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("客户端未取得真实 Chrome PNG", status)
	}
	for _, identity := range []int{0, sessions[1].UID} {
		bounded, stop := context.WithTimeout(context.Background(), time.Second)
		_, err := runGatewayClient(bounded, path, identity, "GET", "/v2/state", "")
		stop()
		if err == nil {
			t.Fatal("错误内核身份仍能请求桌面入口")
		}
	}
	if status, _ := gatewayClient(t, path, uid, "PUT", base, `{"session_id":"other"}`); status != 400 {
		t.Fatal("身份字段注入通过", status)
	}
	access.revoke(sessions[0])
	if status, _ := gatewayClient(t, path, uid, "GET", base+"/image", ""); status != 403 {
		t.Fatal("撤销后仍向客户端发送画面", status)
	}
	// 停服不能按路径盲删普通文件；监督器之后应根据原 socket inode 清理。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	finished <- nil
	if encoded, err := os.ReadFile(path); err != nil || string(encoded) != "replacement" {
		t.Fatal("关闭入口错误删除替换路径", err)
	}
}

func TestGatewayShutdownWaitsForAuthorization(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("TOM_SHARED_BROWSER_TEST") != "1" {
		t.Fatal("必须在专用受限容器内验证窗口入口")
	}
	server, _, _ := gatewayFixture(t)
	entered, stopped := make(chan struct{}), make(chan struct{})
	server.check = func(ctx context.Context, _ computer.ExecutionSession, _ Peer) error {
		close(entered)
		<-ctx.Done()
		defer close(stopped)
		return ErrDenied
	}
	directory, err := os.MkdirTemp("/workspace", "gateway-close-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "browser.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	clientDone := make(chan error, 1)
	go func() {
		_, err := runGatewayClient(ctx, path, server.session.UID, "GET", "/v2/state", "")
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("客户端未进入授权复验")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("窗口入口关闭未返回")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("授权处理仍运行却报告退出")
	}
	if err := <-clientDone; err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal(err)
	}
}

func TestGatewayConnectionLimitAndHeaderDeadline(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("TOM_SHARED_BROWSER_TEST") != "1" {
		t.Fatal("必须在专用受限容器内验证窗口入口")
	}
	server, _, _ := gatewayFixture(t)
	directory, err := os.MkdirTemp("/workspace", "gateway-limit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "browser.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 0, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("窗口入口没有回收空闲连接")
		}
	})
	bounded, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	command := exec.CommandContext(bounded, "/usr/bin/python3", "-I", "-B", "-c", `
import socket, sys, time
def connect():
    s = socket.socket(socket.AF_UNIX)
    s.settimeout(2)
    s.connect(sys.argv[1])
    return s
held = [connect() for _ in range(4)]
try:
    with connect() as excess:
        assert excess.recv(1) == b'', '超限连接没有被拒绝'
    # 不发送 HTTP 头，服务必须在时限后主动释放全部名额。
    for connection in held:
        assert connection.recv(1) == b'', '未完成请求头的连接没有超时'
    with connect() as recovered:
        recovered.sendall(b'GET /v2/state HTTP/1.1\r\nHost: browser.local\r\nConnection: close\r\n\r\n')
        assert recovered.recv(4096).startswith(b'HTTP/1.1 200'), '超时连接释放后无法继续请求'
    print('连接容量与读取期限验证通过')
finally:
    for connection in held:
        connection.close()
`, path)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(server.session.UID), Gid: uint32(server.session.GID)}}
	if encoded, err := command.CombinedOutput(); err != nil {
		t.Fatal("窗口入口容量或读取期限验证失败", err, string(encoded))
	}
}
