//go:build linux && cgroupintegration

package processgroup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// TestKernelServiceGroups 仅在专用 Linux 测试节点显式运行，不接受普通文件模拟内核。
// 容器须由测试部署单独创建并提供实际主进程；只变更新生成的测试服务和会话子组。
func TestKernelServiceGroups(t *testing.T) {
	if os.Getenv("TOM_TEST_CGROUP_KERNEL") != "1" || os.Geteuid() != 0 {
		t.Fatal("真实资源组验收要求专用 Linux root 测试节点和显式启用")
	}
	pid, err := strconv.Atoi(os.Getenv("TOM_TEST_CGROUP_CONTAINER_PID"))
	if err != nil {
		t.Fatal("缺少测试容器实际主进程")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	parent, err := LocateContainer(ctx, os.Getenv("TOM_TEST_CGROUP_CONTAINER_ID"), pid)
	if err != nil {
		t.Fatal(err)
	}
	options := Options{GracePeriod: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond, MaximumProcesses: 64}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	services, err := NewServices(options, logger)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := New(options, logger)
	if err != nil {
		t.Fatal(err)
	}
	browser := ServiceReference{Container: parent, ID: ids.New(), Kind: BrowserService}
	probe := ServiceReference{Container: parent, ID: ids.New(), Kind: ProbeService}
	changedParent := browser
	changedParent.Container.Inode++
	if services.Create(ctx, changedParent) == nil || services.Drain(ctx, changedParent) == nil {
		t.Fatal("替换父目录身份仍可变更真实资源组")
	}
	if files, err := services.Controls(ctx, changedParent); len(files) != 0 || err == nil {
		closeKernelControls(t, files)
		t.Fatal("替换父目录身份仍可取得真实控制入口")
	}
	for _, reference := range []ServiceReference{browser, probe} {
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			if err := services.Drain(cleanup, reference); err != nil {
				t.Error("测试服务组未完全释放", err)
			}
		})
		if err = services.Create(ctx, reference); err != nil {
			t.Fatal(err)
		}
	}
	session := computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: parent.ContainerID, Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	task, err := groups.Locate(ctx, session, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := groups.Drain(cleanup, task); err != nil {
			t.Error("测试会话组未完全释放", err)
		}
	})
	if err = groups.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	browserFiles, err := services.Controls(ctx, browser)
	if err != nil {
		t.Fatal(err)
	}
	closeKernelControls(t, browserFiles)
	probeFiles, err := services.Controls(ctx, probe)
	if err != nil {
		t.Fatal(err)
	}
	closeKernelControls(t, probeFiles)
	taskFiles, err := groups.Controls(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	closeKernelControls(t, taskFiles)
	if err = ValidateServiceControls(browser, browserFiles); err != nil {
		t.Fatal("真实服务入口校验失败", err)
	}
	if err = ValidateControls(task, taskFiles); err != nil {
		t.Fatal("真实任务入口校验失败", err)
	}
	if ValidateServiceControls(probe, browserFiles) == nil || ValidateServiceControls(browser, []*os.File{browserFiles[1], browserFiles[0], browserFiles[2]}) == nil || ValidateServiceControls(browser, []*os.File{taskFiles[0], taskFiles[2], taskFiles[3]}) == nil {
		t.Fatal("跨服务、错序或 Bot 入口被接受")
	}
	browserPID, browserExit := startKernelChild(t, browserFiles[0], false)
	probePID, probeExit := startKernelChild(t, probeFiles[0], false)
	taskPID, taskExit := startKernelChild(t, taskFiles[0], false)
	if member, err := services.Contains(ctx, browser, browserPID); !member || err != nil {
		t.Fatal("实际浏览器进程归属未确认", err)
	}
	for _, otherPID := range []int{probePID, taskPID, pid} {
		if member, err := services.Contains(ctx, browser, otherPID); member || err != nil {
			t.Fatal("错误进程被归属共享浏览器", err)
		}
	}
	if err = groups.Drain(ctx, task); err != nil {
		t.Fatal(err)
	}
	assertKernelChildExit(t, taskExit, true)
	for reference, childPID := range map[ServiceReference]int{browser: browserPID, probe: probePID} {
		if member, err := services.Contains(ctx, reference, childPID); !member || err != nil {
			t.Fatal("Bot 回收影响同级基础服务", err)
		}
	}
	// 已发放的旧入口仍保持打开，真实迟到写入也必须被内核拒绝。
	_, lateExit := startKernelChild(t, taskFiles[0], true)
	assertKernelChildExit(t, lateExit, false)
	if err = services.Drain(ctx, probe); err != nil {
		t.Fatal(err)
	}
	assertKernelChildExit(t, probeExit, true)
	if member, err := services.Contains(ctx, browser, browserPID); !member || err != nil {
		t.Fatal("探针回收影响共享浏览器", err)
	}
	if err = services.Drain(ctx, browser); err != nil {
		t.Fatal(err)
	}
	assertKernelChildExit(t, browserExit, true)
	if member, err := services.Contains(ctx, browser, browserPID); member || err != nil {
		t.Fatal("已回收服务仍被确认为存活", err)
	}
	_, lateExit = startKernelChild(t, browserFiles[0], true)
	assertKernelChildExit(t, lateExit, false)
	if err = services.Drain(ctx, browser); err != nil {
		t.Fatal("已删除服务组重复回收失败", err)
	}
	t.Log("实际内核服务入组、描述符边界、Bot 与服务定向回收及迟到入组拒绝通过")
}

func closeKernelControls(t *testing.T, files []*os.File) {
	t.Helper()
	t.Cleanup(func() {
		for _, file := range files {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
}

func startKernelChild(t *testing.T, membership *os.File, expectDenial bool) (int, <-chan error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKernelServiceGroupChild$", "-test.timeout=9s")
	command.Env = append(os.Environ(), "TOM_TEST_CGROUP_CHILD=1", "TOM_TEST_CGROUP_DENIAL="+strconv.FormatBool(expectDenial))
	command.ExtraFiles = []*os.File{membership, writer}
	command.WaitDelay = time.Second
	if err = command.Start(); err != nil {
		cancel()
		t.Fatal(errors.Join(err, reader.Close(), writer.Close()))
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { done <- command.Wait(); close(finished) }()
	t.Cleanup(func() { cancel(); <-finished })
	err = writer.Close()
	if err == nil {
		err = reader.SetReadDeadline(time.Now().Add(2 * time.Second))
	}
	var ready [1]byte
	if err == nil {
		_, err = io.ReadFull(reader, ready[:])
	}
	if err = errors.Join(err, reader.Close()); err != nil || ready[0] != 1 {
		t.Fatal("内核验收子进程未完成入组或拒绝核验", err)
	}
	return command.Process.Pid, done
}

func assertKernelChildExit(t *testing.T, done <-chan error, killed bool) {
	t.Helper()
	select {
	case err := <-done:
		if !killed && err != nil {
			t.Fatal("迟到入组没有明确拒绝", err)
		}
		if killed {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal("回收未通过内核终止实际进程", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("忽略正常退出的子进程未被强制终止", err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("内核回收后仍有未退出子进程")
	}
}

// TestKernelServiceGroupChild 只用于本文件的固定测试进程，不执行外部命令或访问业务文件。
func TestKernelServiceGroupChild(t *testing.T) {
	if os.Getenv("TOM_TEST_CGROUP_CHILD") != "1" {
		t.Fatal("内核辅助进程必须由专用验收入口启动")
	}
	membership := os.NewFile(3, "membership")
	ready := os.NewFile(4, "ready")
	if membership == nil || ready == nil {
		t.Fatal("缺少内核验收入口")
	}
	signal.Ignore(syscall.SIGTERM)
	count, err := membership.WriteString("0")
	denied := os.Getenv("TOM_TEST_CGROUP_DENIAL") == "true"
	if closeErr := membership.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if denied && err == nil || !denied && (err != nil || count != 1) {
		t.Fatal("内核入组结果不符", err)
	}
	if count, err = ready.Write([]byte{1}); err != nil || count != 1 {
		t.Fatal("发送内核验收就绪失败", err)
	}
	if err = ready.Close(); err != nil {
		t.Fatal(err)
	}
	if !denied {
		<-time.After(8 * time.Second)
		t.Fatal("内核验收进程未被受管回收")
	}
}
