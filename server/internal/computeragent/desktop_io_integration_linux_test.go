//go:build linux && agentintegration && !cgo

package computeragent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func graphicalTestCommand(t *testing.T, session computer.ExecutionSession, program string, arguments ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, program, arguments...)
	environment, err := session.DesktopEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Dir = "/workspace"
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
	return command
}

func testDesktopTerminal(t *testing.T, session computer.ExecutionSession, title string) {
	t.Helper()
	// 桌面测试使用真实独立 UID；本测试启动器仍不代表生产 cgroup 入组。
	command := graphicalTestCommand(t, session, "/usr/bin/xfce4-terminal", "--disable-server", "--title", title, "--geometry", "80x20+50+50", "--command", "bash --noprofile --norc")
	// 长驻窗口的期限覆盖整个单项验收，短命令仍使用五秒期限。
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	window := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
	window.Env, window.Dir, window.SysProcAttr = command.Env, command.Dir, command.SysProcAttr
	window.SysProcAttr.Setpgid = true
	if err := window.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- window.Wait() }()
	t.Cleanup(func() {
		if err := syscall.Kill(-window.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
		select {
		case err := <-done:
			var exited *exec.ExitError
			if err != nil && !errors.As(err, &exited) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("验收窗口未在期限内正常退出")
			if err := syscall.Kill(-window.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Error(err)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("验收窗口未退出")
			}
		}
		cancel()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		encoded, err := graphicalTestCommand(t, session, "/usr/bin/xdotool", "search", "--onlyvisible", "--name", title).Output()
		if err == nil {
			windows := strings.Fields(string(encoded))
			if len(windows) != 1 {
				t.Fatal("验收窗口数量异常")
			}
			if err := graphicalTestCommand(t, session, "/usr/bin/xdotool", "windowactivate", "--sync", windows[0]).Run(); err != nil {
				t.Fatal(err)
			}
			return
		}
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 1 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("验收终端未出现")
}

func TestKernelDesktopIOIndependentScreens(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("桌面输入输出验收必须运行在专用容器")
	}
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	second, _ := desktopCommand(fixture.command, 2)
	sessions := []computer.ExecutionSession{fixture.command.Session, second.Session}
	accesses := make([]*desktopAccess, 2)
	for index, session := range sessions {
		desktop, _, _ := testGraphicalDesktop(t, session)
		waitGraphicalStatus(t, desktop, "ready")
		access := newDesktopAccess(session, func(ctx context.Context) (net.Conn, error) { return openDesktopConnection(ctx, session, "/") })
		accesses[index] = access
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := access.close(ctx); err != nil {
				t.Error(err)
			}
		})
		testDesktopTerminal(t, session, "桌面验收"+strconv.Itoa(index+1))
	}
	var pending sync.WaitGroup
	for index, session := range sessions {
		pending.Add(1)
		go func() {
			defer pending.Done()
			number := strconv.Itoa(index + 1)
			text := "printf '机器人" + number + "' > /workspace/desktop-io-" + session.SessionID + "\n"
			input := computer.DesktopInput{Session: session, Sequence: 1}
			for _, character := range text {
				key := uint32(character)
				if character == '\n' {
					key = 0xff0d
				} else if key > 255 {
					key |= 0x01000000
				}
				input.Events = append(input.Events, computer.DesktopEvent{Kind: "key", Key: key, Down: true}, computer.DesktopEvent{Kind: "key", Key: key})
			}
			input.Events = append(input.Events, computer.DesktopEvent{Kind: "pointer", X: 100 * (index + 1), Y: 150 * (index + 1)})
			receipt, err := accesses[index].send(context.Background(), input)
			if err != nil || receipt.State != "sent" {
				t.Error("实际桌面输入失败", receipt.State, err)
			}
		}()
	}
	pending.Wait()
	images := make([][]byte, 2)
	for index, session := range sessions {
		path := "/workspace/desktop-io-" + session.SessionID
		deadline := time.Now().Add(5 * time.Second)
		for {
			encoded, err := os.ReadFile(path)
			if err == nil && string(encoded) == "机器人"+strconv.Itoa(index+1) {
				break
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("中文键盘输入未落在所属终端", index+1, string(encoded), err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		coordinates, err := graphicalTestCommand(t, session, "/usr/bin/xdotool", "getmouselocation", "--shell").Output()
		if err != nil || !strings.Contains(string(coordinates), "X="+strconv.Itoa(100*(index+1))+"\n") || !strings.Contains(string(coordinates), "Y="+strconv.Itoa(150*(index+1))+"\n") {
			t.Fatal("鼠标串到其他桌面", err)
		}
		if err := graphicalTestCommand(t, session, "/usr/bin/xdotool", "search", "--onlyvisible", "--name", "桌面验收"+strconv.Itoa(2-index)).Run(); err == nil {
			t.Fatal("本桌面可见另一 Bot 窗口")
		} else {
			var absent *exec.ExitError
			if !errors.As(err, &absent) || absent.ExitCode() != 1 {
				t.Fatal("无法确认另一 Bot 窗口是否存在", err)
			}
		}
		request := computer.DesktopCapture{Session: session, Sequence: 1}
		metadata, err := accesses[index].capture(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		read := computer.DesktopImageRead{DesktopCapture: request, MaximumBytes: computer.MaximumDesktopChunkBytes}
		for read.Offset < metadata.Size {
			chunk, err := accesses[index].read(context.Background(), read)
			if err != nil || chunk.Validate(read) != nil {
				t.Fatal(err)
			}
			images[index] = append(images[index], chunk.Bytes...)
			read.Offset += len(chunk.Bytes)
		}
		digest := sha256.Sum256(images[index])
		if hex.EncodeToString(digest[:]) != metadata.SHA256 {
			t.Fatal("真实截图摘要不符")
		}
		frame, err := png.Decode(bytes.NewReader(images[index]))
		if err != nil || frame.Bounds().Dx() != 1280 || frame.Bounds().Dy() != 800 {
			t.Fatal("实际截图不是固定尺寸", err)
		}
		if err := os.WriteFile("/workspace/desktop-io-"+strconv.Itoa(index+1)+".png", images[index], 0600); err != nil {
			t.Fatal(err)
		}
		if report := os.Getenv("TOM_DESKTOP_IO_REPORT"); report != "" {
			if err := os.WriteFile(filepath.Join(report, "desktop-io-"+strconv.Itoa(index+1)+".png"), images[index], 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := accesses[1-index].capture(context.Background(), request); err == nil {
			t.Fatal("另一桌面获得本 Bot 截图")
		}
	}
	if bytes.Equal(images[0], images[1]) {
		t.Fatal("两个独立窗口返回同一画面")
	}
	accesses[0].stop()
	if _, err := accesses[0].capture(context.Background(), computer.DesktopCapture{Session: sessions[0], Sequence: 2}); err == nil {
		t.Fatal("撤销后仍可截图")
	}
	if _, err := accesses[1].capture(context.Background(), computer.DesktopCapture{Session: sessions[1], Sequence: 2}); err != nil {
		t.Fatal("另一 Bot 被连带撤销", err)
	}
}
