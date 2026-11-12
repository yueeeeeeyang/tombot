//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// testGraphicalDesktop 使用真实降权桌面检验监督和探测；测试不声称完成业务 cgroup 入组。
func testGraphicalDesktop(t *testing.T, session computer.ExecutionSession) (*managedDesktop, *desktopDirectory, <-chan string) {
	t.Helper()
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	desktop, failures := testGraphicalDesktopWithDirectory(t, session, directory)
	return desktop, directory, failures
}

func testGraphicalDesktopWithDirectory(t *testing.T, session computer.ExecutionSession, directory *desktopDirectory) (*managedDesktop, <-chan string) {
	t.Helper()
	command := exec.Command("/usr/bin/python3", "-I", "-B", "/usr/lib/tombot/desktop.py", "--runtime", session.RuntimeDirectory(), "--display", strconv.Itoa(session.DisplayNumber))
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	failures := make(chan string, 2)
	desktop := &managedDesktop{status: computer.DesktopStatus{Session: session, State: "starting"}, command: command, done: make(chan struct{}), probed: make(chan struct{}), stop: make(chan struct{}), onFailure: func(code string) { failures <- code }}
	desktop.supervise(func(ctx context.Context) (bool, error) {
		ready, err := directory.readyFile()
		if !ready || err != nil {
			return ready, err
		}
		// 测试直接降权的进程位于此临时容器根组，生产探测固定使用授权会话的 Shell 组。
		err = probeDesktopSocket(ctx, session, "/")
		return err == nil, err
	})
	t.Cleanup(func() {
		desktop.stoppingNow()
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := desktop.closeAfterDrain(ctx); err != nil {
			t.Error(err)
			if killError := command.Process.Kill(); killError != nil && !errors.Is(killError, os.ErrProcessDone) {
				t.Error(killError)
			}
		}
	})
	return desktop, failures
}

func waitGraphicalStatus(t *testing.T, desktop *managedDesktop, state string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		status := desktop.snapshot()
		if status.State == state {
			return
		}
		if status.State == "failed" {
			t.Fatal("图形会话启动失败", status.ErrorCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("图形会话未达到预期状态", state, desktop.snapshot())
}

func TestKernelGraphicalSupervisorAndPrivateProbe(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("真实图形测试必须运行在专用容器")
	}
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	first, firstDirectory, firstFailures := testGraphicalDesktop(t, fixture.command.Session)
	secondCommand, _ := desktopCommand(fixture.command, 2)
	second, _, secondFailures := testGraphicalDesktop(t, secondCommand.Session)
	waitGraphicalStatus(t, first, "ready")
	waitGraphicalStatus(t, second, "ready")
	if first.snapshot().Validate() != nil || second.snapshot().Validate() != nil {
		t.Fatal("真实桌面状态不符合固定尺寸约定")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probeDesktopSocket(ctx, fixture.command.Session, "/another-session/shell"); err == nil {
		t.Fatal("错误资源组通过桌面连接校验")
	}
	wrong := secondCommand.Session
	wrong.UID = fixture.command.Session.UID
	if err := probeDesktopSocket(ctx, wrong, "/"); err == nil {
		t.Fatal("跨 Bot 图形连接通过 UID 校验")
	}
	if err := os.WriteFile(fixture.command.Session.RuntimeDirectory()+"/ready.json", []byte(`{"display":1,"width":1920,"height":800,"protocol":"x11"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := firstDirectory.readyFile(); err == nil {
		t.Fatal("接受了伪造分辨率标记")
	}
	if err := first.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-firstFailures:
		if code != "desktop_exited" {
			t.Fatal("意外退出原因不符", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("图形核心退出未报告")
	}
	if first.snapshot().State != "failed" {
		t.Fatal("退出后仍报告桌面可用")
	}
	if second.snapshot().State != "ready" {
		t.Fatal("关闭一个桌面影响了另一个状态")
	}
	probe, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelProbe()
	if err := probeDesktopSocket(probe, secondCommand.Session, "/"); err != nil {
		t.Fatal("另一桌面实际连接受到影响", err)
	}
	select {
	case code := <-secondFailures:
		t.Fatal("另一桌面被误报失败", code)
	default:
	}
}

func TestKernelDesktopRejectsFakeMembership(t *testing.T) {
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, fixture.command.Session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	membership, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(membership.Close(), reader.Close(), writer.Close()); err != nil {
			t.Error(err)
		}
	}()
	scope := &kernelScope{directory: directory, membershipUsed: true}
	failures := make(chan string, 2)
	status, err := scope.StartDesktop(context.Background(), fixture.reference, fixture.command.Session, membership, reader, func(code string) { failures <- code })
	if err != nil || status.Validate() != nil {
		t.Fatal(status, err)
	}
	select {
	case <-failures:
	case <-time.After(3 * time.Second):
		t.Fatal("伪造资源组没有阻止图形启动")
	}
	first := scope.desktop
	status, err = scope.StartDesktop(context.Background(), fixture.reference, fixture.command.Session, membership, reader, func(string) { t.Error("失败图形会话被重新启动") })
	if err != nil || status.State != "failed" || scope.desktop != first {
		t.Fatal("失败桌面没有保留原结果", status, err)
	}
	if _, err := os.Stat(fixture.command.Session.RuntimeDirectory() + "/desktop.sock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("伪造入组入口启动了 X11", err)
	}
	if err := scope.Close(); err == nil {
		t.Fatal("未排空图形会话提前清理")
	}
	scope.drained = true
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}
