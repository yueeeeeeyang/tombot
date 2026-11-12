//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/terminalio"
)

// testManagedTerminal 只用于验证真实 PTY 读写和生命周期，不把跳过 cgroup 的测试启动器带入生产。
func testManagedTerminal(t *testing.T, script string) *managedTerminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	master, err = terminalio.PollablePTY(master)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", script)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	terminal := &managedTerminal{master: master, command: command, output: terminalio.NewBuffer(terminalBufferBytes), exitCode: -1, done: make(chan struct{}), readDone: make(chan struct{})}
	go terminal.readOutput()
	go terminal.wait()
	t.Cleanup(func() {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := terminal.closeAfterDrain(ctx); err != nil {
			t.Error(err)
		}
	})
	return terminal
}

func awaitTerminal(t *testing.T, terminal *managedTerminal, expected string) computer.TerminalOutput {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, err := terminal.read(0, computer.MaximumTerminalReadBytes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.Text, expected) {
			return output
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("终端没有返回预期输出", expected)
	return computer.TerminalOutput{}
}

func TestKernelManagedTerminalIOAndIndependentExit(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("真实终端测试必须运行在专用容器")
	}
	first := testManagedTerminal(t, `stty -echo; printf READY; IFS= read -r first; printf '<%s>' "$first"; IFS= read -r second; printf '[%s]' "$second"; stty size; exit 7`)
	second := testManagedTerminal(t, `stty -echo; printf SECOND; IFS= read -r line; printf 'received:%s' "$line"`)
	awaitTerminal(t, first, "READY")
	awaitTerminal(t, second, "SECOND")
	if err := terminalio.ResizePTY(first.master, 100, 30); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	receipt, err := first.write(ctx, 1, "你好\n")
	if err != nil || receipt.WrittenBytes != 7 || receipt.ErrorCode != "" {
		t.Fatal(receipt, err)
	}
	replayed, err := first.write(ctx, 1, "你好\n")
	if err != nil || replayed != receipt {
		t.Fatal("重复输入未返回原回执", err)
	}
	if _, err := first.write(ctx, 1, "changed\n"); err == nil {
		t.Fatal("重复序号允许更换输入")
	}
	if _, err := first.write(ctx, 3, "skipped\n"); err == nil {
		t.Fatal("接受了跳号输入")
	}
	if _, err := first.write(ctx, 2, "下一行\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	case <-time.After(3 * time.Second):
		t.Fatal("终端程序退出未确认")
	}
	output, err := first.read(0, 4096)
	if err != nil || !output.Exited || output.ExitCode != 7 || !strings.Contains(output.Text, "<你好>[下一行]30 100") {
		t.Fatal("中文、输入去重、尺寸或退出码错误", output, err)
	}
	if _, err := second.write(ctx, 1, "still-alive\n"); err != nil {
		t.Fatal("一终端退出影响了另一个", err)
	}
	awaitTerminal(t, second, "received:still-alive")
	if _, err := first.read(output.Cursor+1, 4096); err == nil {
		t.Fatal("接受了超前输出游标")
	}
}

func TestKernelManagedTerminalPartialInputAndBuffer(t *testing.T) {
	terminal := testManagedTerminal(t, `stty -echo -icanon; printf READY; sleep 10`)
	awaitTerminal(t, terminal, "READY")
	var sequence int64
	var receipt computer.TerminalReceipt
	for sequence = 1; sequence <= 16; sequence++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		var err error
		receipt, err = terminal.write(ctx, sequence, strings.Repeat("x", computer.MaximumTerminalInputBytes))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ErrorCode != "" {
			break
		}
	}
	if receipt.ErrorCode != "terminal_write_incomplete" {
		t.Fatal("未读取的 PTY 输入没有在期限内失败")
	}
	before := time.Now()
	replayed, err := terminal.write(context.Background(), sequence, strings.Repeat("x", computer.MaximumTerminalInputBytes))
	if err != nil || replayed != receipt || time.Since(before) > time.Second {
		t.Fatal("部分写入被再次执行", err, replayed)
	}
	terminal.mu.Lock()
	terminal.output.Append([]byte(strings.Repeat("中文", terminalBufferBytes/3)))
	terminal.mu.Unlock()
	output, err := terminal.read(0, 4)
	if err != nil || !output.Truncated || !utf8.ValidString(output.Text) || len(output.Text) > 4 {
		t.Fatal("有界中文缓冲错误", output, err)
	}
}

func TestKernelManagedTerminalRejectsFakeMembership(t *testing.T) {
	fixture := newFixture(t)
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, fixture.command.Session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	request := computer.TerminalStart{TerminalAddress: computer.TerminalAddress{Session: fixture.command.Session, OperationID: fixture.command.Session.RunID}, Command: computer.ProcessCommand{Program: "/bin/sh", WorkingDirectory: "/workspace", Arguments: []string{"-c", "printf SHOULD_NOT_EXECUTE"}}, Columns: 80, Rows: 24}
	membership, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := membership.Close(); err != nil {
			t.Error(err)
		}
	}()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(reader.Close(), writer.Close()); err != nil {
			t.Error(err)
		}
	}()
	scope := &kernelScope{directory: directory, membershipUsed: true}
	output, err := scope.StartTerminal(context.Background(), fixture.reference, request, membership, reader)
	if err != nil {
		t.Fatal(err)
	}
	terminal := scope.terminals[request.OperationID].terminal
	select {
	case <-terminal.done:
	case <-time.After(3 * time.Second):
		t.Fatal("固定引导没有退出")
	}
	output, err = scope.ReadTerminal(context.Background(), computer.TerminalRead{TerminalAddress: request.TerminalAddress, MaximumBytes: 4096})
	if err != nil || !output.Exited || output.ExitCode == 0 || strings.Contains(output.Text, "SHOULD_NOT_EXECUTE") {
		t.Fatal("伪造入组入口执行了用户程序", output, err)
	}
	if _, err := scope.StartTerminal(context.Background(), fixture.reference, request, membership, reader); err != nil || len(scope.terminals) != 1 {
		t.Fatal("重复启动重新分配了终端", err)
	}
	changed := request
	changed.Columns++
	if _, err := scope.StartTerminal(context.Background(), fixture.reference, changed, membership, reader); err == nil {
		t.Fatal("同一启动标识允许更换内容")
	}
	if err := scope.Close(); err == nil {
		t.Fatal("终端退出被误认为整个资源组已排空")
	}
	scope.drained = true
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}
