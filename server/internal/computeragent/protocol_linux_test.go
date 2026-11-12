//go:build linux

package computeragent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/agentchannel"
)

func protocolPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var pair []*net.UnixConn
	for _, fd := range fds {
		file := os.NewFile(uintptr(fd), "agent-protocol-test")
		connection, err := net.FileConn(file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		value, ok := connection.(*net.UnixConn)
		if !ok {
			t.Fatal("协议测试连接类型错误")
		}
		pair = append(pair, value)
	}
	return pair[0], pair[1]
}

func sameTestProcess(peer agentchannel.Peer) error {
	if peer.PID != int32(os.Getpid()) || peer.UID != uint32(os.Geteuid()) || peer.GID != uint32(os.Getegid()) {
		return errors.New("测试对端内核身份不符")
	}
	return nil
}

func TestKernelSessionControlProtocol(t *testing.T) {
	fixture := newFixture(t)
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	var pending sync.WaitGroup
	client, err := NewClient(func(ctx context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		pending.Add(1)
		go func() {
			defer pending.Done()
			call, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- server.serveConnection(call, right)
		}()
		return left, nil
	}, sameTestProcess, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pending.Wait)
	check := func(wantError bool) {
		t.Helper()
		select {
		case err := <-results:
			if (err != nil) != wantError {
				t.Fatal("代理协议处理结果不符", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("代理协议处理未结束")
		}
	}
	original := time.Now().Add(fixture.command.Lifetime)
	deadline, err := agentchannel.FromDeadline(original)
	if err != nil {
		t.Fatal(err)
	}
	files := fixture.files(t)
	defer func() {
		if err := agentchannel.CloseFiles(files); err != nil {
			t.Error(err)
		}
	}()
	if observation, err := client.Open(context.Background(), Grant{fixture.command, fixture.reference, deadline}, files); err != nil || !observation.Matches(fixture.command, "active") {
		t.Fatal("真实描述符通道开启失败", err)
	}
	check(false)
	if fixture.guard.permit.deadline.After(original) {
		t.Fatal("RPC 延长了节点原始期限")
	}
	renewed := fixture.command
	renewed.Revision++
	deadline, err = agentchannel.FromDeadline(time.Now().Add(renewed.Lifetime))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Renew(context.Background(), Renewal{renewed, deadline}); err != nil {
		t.Fatal(err)
	}
	check(false)
	expires := fixture.guard.permit.deadline
	if _, err := client.Renew(context.Background(), Renewal{renewed, deadline}); err != nil {
		t.Fatal(err)
	}
	check(false)
	if !fixture.guard.permit.deadline.Equal(expires) {
		t.Fatal("RPC 重复续约延长租约")
	}
	closed := renewed
	closed.Revision++
	if observation, err := client.Close(context.Background(), closed); err != nil || !observation.Matches(closed, "closed") {
		t.Fatal("真实描述符通道关闭失败", err)
	}
	check(false)
	if _, err := client.Renew(context.Background(), Renewal{renewed, deadline}); err == nil {
		t.Fatal("旧续约经 RPC 复活了授权")
	}
	check(true)
}

func TestKernelHostPeerRequirement(t *testing.T) {
	if err := RequireHostPeer(agentchannel.Peer{}); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []agentchannel.Peer{{PID: 1}, {PID: 200}, {UID: 10000}, {GID: 10000}} {
		if err := RequireHostPeer(peer); err == nil {
			t.Fatal("容器内进程或非 root 身份被认为是宿主控制者")
		}
	}
}

func TestKernelControlServerCancellationBoundsPendingConnections(t *testing.T) {
	fixture := newFixture(t)
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "control.sock"), Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	for range 3 {
		connection, err := net.DialUnix("unixpacket", nil, listener.Addr().(*net.UnixAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("停止监听未传播取消", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("控制服务停机被空连接阻塞")
	}
}
