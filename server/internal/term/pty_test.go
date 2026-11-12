package term

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/terminalio"
	"io.yueyang/tom/internal/testkit"
)

func terminalFixture(t *testing.T) (*Manager, *Session) {
	t.Helper()
	manager := NewManager(context.Background())
	manager.terminationGrace = 50 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error("终端回收失败", err)
		}
	})
	home := shellHome(t)
	session, err := manager.Open(context.Background(), SessionOptions{ID: "session", Owner: "user", Cwd: home, Home: home, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	return manager, session
}

func terminalReadUntil(t *testing.T, session *Session, cursor int64, marker string) (string, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output strings.Builder
	for {
		part, err := session.Read(ctx, cursor, MaxSessionReadBytes, true)
		if err != nil {
			t.Fatal("终端输出等待失败", err)
		}
		if !utf8.ValidString(part.Data) || part.ErrorCode != "" {
			t.Fatal("终端输出错误", part.ErrorCode)
		}
		output.WriteString(part.Data)
		cursor = part.Cursor
		if strings.Contains(output.String(), marker) {
			return output.String(), cursor
		}
		if part.Exited && !part.HasMore {
			t.Fatal("终端提前退出", part.ExitCode)
		}
	}
}

func terminalWrite(t *testing.T, session *Session, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := session.Write(ctx, text); err != nil {
		t.Fatal("终端写入失败", err)
	}
}

func TestTerminalSharedOutputResizeExitAndEnvironment(t *testing.T) {
	privateValue := testkit.Random(t)
	t.Setenv("TOM_TEST_PRIVATE", privateValue)
	_, session := terminalFixture(t)
	terminalWrite(t, session, "stty -echo; printf 'rea%s' 'dy\\n'\r")
	_, cursor := terminalReadUntil(t, session, 0, "ready")
	if err := session.Resize(context.Background(), 123, 37); err != nil {
		t.Fatal(err)
	}
	terminalWrite(t, session, "printf '标记'; stty size; printf '%s' \"${TOM_TEST_PRIVATE-unset}\"; printf '结束'; exit 7\r")
	output, end := terminalReadUntil(t, session, cursor, "结束")
	if !strings.Contains(output, "37 123") || !strings.Contains(output, "unset") || strings.Contains(output, privateValue) || !strings.Contains(output, "标记") {
		t.Fatal("环境、尺寸或中文输出不符合预期")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		part, err := session.Read(ctx, end, MaxSessionReadBytes, true)
		if err != nil {
			t.Fatal(err)
		}
		end = part.Cursor
		if part.Exited {
			if part.ExitCode != 7 || part.ErrorCode != "" {
				t.Fatal("退出状态错误", part.ExitCode, part.ErrorCode)
			}
			break
		}
	}
	// 用户连接与模型读取持有各自的游标，后来的观察者仍能看到相同输出。
	replay, err := session.Read(ctx, cursor, MaxSessionReadBytes, false)
	if err != nil || !strings.Contains(replay.Data, "标记") || !replay.Exited {
		t.Fatal("第二个观察者未收到输出", err)
	}
	if err = session.Write(ctx, "true\r"); err == nil {
		t.Fatal("退出后仍接受输入")
	}
}

func TestTerminalRequestDisconnectAndRootCancellation(t *testing.T) {
	root, stop := context.WithCancel(context.Background())
	manager := NewManager(root)
	manager.terminationGrace = 25 * time.Millisecond
	defer stop()
	home := shellHome(t)
	request, disconnect := context.WithCancel(context.Background())
	session, err := manager.Open(request, SessionOptions{ID: "detached", Owner: "user", Cwd: home, Home: home, Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	disconnect()
	terminalWrite(t, session, "printf 'still%s' 'alive'\r")
	terminalReadUntil(t, session, 0, "stillalive")
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = session.Close(ctx); err != nil {
		t.Fatal("根上下文取消未回收", err)
	}
	if _, err = manager.Open(context.Background(), SessionOptions{ID: "another", Owner: "user", Cwd: home, Home: home, Columns: 80, Rows: 24}); err == nil {
		t.Fatal("停止后仍允许创建")
	}
}

func TestTerminalCloseKillsSeparateJobControlGroups(t *testing.T) {
	_, session := terminalFixture(t)
	terminalWrite(t, session, "stty -echo; printf 'rea%s' 'dy'\r")
	_, cursor := terminalReadUntil(t, session, 0, "ready")
	terminalWrite(t, session, "(trap '' TERM HUP; while :; do sleep 1; done) & printf 'child=%s;done' \"$!\"\r")
	output, _ := terminalReadUntil(t, session, cursor, ";done")
	start := strings.LastIndex(output, "child=") + len("child=")
	end := strings.Index(output[start:], ";done") + start
	pid, err := strconv.Atoi(output[start:end])
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group == session.command.Process.Pid {
		t.Fatal("测试没有创建独立作业组", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err = syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后台作业仍存在", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminalLimitsValidationAndConcurrentWrites(t *testing.T) {
	manager, session := terminalFixture(t)
	manager.maximumPerOwner = 1
	home := shellHome(t)
	options := SessionOptions{ID: "another", Owner: "user", Cwd: home, Home: home, Columns: 80, Rows: 24}
	_, err := manager.Open(context.Background(), options)
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "TERMINAL_LIMIT" {
		t.Fatal("会话数限制未生效", err)
	}
	options.Owner, options.Cwd = "bot:other", filepath.Join(home, "missing")
	if _, err = manager.Open(context.Background(), options); err == nil {
		t.Fatal("无效目录仍创建会话")
	}
	if _, err = manager.Get(options.ID); err == nil {
		t.Fatal("失败启动仍占用会话")
	}
	for _, size := range [][2]int{{1, 24}, {501, 24}, {80, 0}, {80, 301}} {
		if err = session.Resize(context.Background(), size[0], size[1]); err == nil {
			t.Fatal("无效尺寸被接受")
		}
	}
	for _, text := range []string{"", strings.Repeat("a", MaxSessionInputBytes+1), string([]byte{255})} {
		if err = session.Write(context.Background(), text); err == nil {
			t.Fatal("无效输入被接受")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = session.Write(canceled, "false\r"); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的输入没有被拒绝", err)
	}
	terminalWrite(t, session, "stty -echo; printf 'rea%s' 'dy'\r")
	_, cursor := terminalReadUntil(t, session, 0, "ready")
	errorsFound := make(chan error, 8)
	var writers sync.WaitGroup
	for index := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			errorsFound <- session.Write(context.Background(), fmt.Sprintf("printf '<%d>'\r", index))
		}()
	}
	writers.Wait()
	close(errorsFound)
	for err = range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	terminalWrite(t, session, "printf 'com%s' 'pleted'\r")
	output, _ := terminalReadUntil(t, session, cursor, "completed")
	for index := range 8 {
		if !strings.Contains(output, fmt.Sprintf("<%d>", index)) {
			t.Fatal("并发输入发生交错", index)
		}
	}
	if err = manager.Remove(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	options.Owner, options.Cwd = "user", home
	if _, err = manager.Open(context.Background(), options); err != nil {
		t.Fatal("关闭后未释放配额", err)
	}
}

func TestTerminalCursorCancellationAndBoundedUTF8Buffer(t *testing.T) {
	buffer := terminalio.NewBuffer(11)
	buffer.Append([]byte("一二三四五六"))
	text, next, truncated := buffer.Read(0, 4)
	if text != "四" || next != 12 || !truncated {
		t.Fatal("环形缓冲字符边界错误", text, next, truncated)
	}
	text, next, truncated = buffer.Read(next, 8)
	if text != "五六" || next != 18 || truncated {
		t.Fatal("环形缓冲续传错误", text, next, truncated)
	}
	buffer.Append([]byte("七八"))
	text, next, truncated = buffer.Read(18, 8)
	if text != "七八" || next != 24 || truncated {
		t.Fatal("环形缓冲跨尾部读取错误", text, next, truncated)
	}
	_, session := terminalFixture(t)
	if _, err := session.Read(context.Background(), -1, 10, false); err == nil {
		t.Fatal("负游标被接受")
	}
	if _, err := session.Read(context.Background(), 1<<30, 10, false); err == nil {
		t.Fatal("越界游标被接受")
	}
	terminalWrite(t, session, "stty -echo; printf 'rea%s' 'dy'\r")
	_, cursor := terminalReadUntil(t, session, 0, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	for {
		part, err := session.Read(ctx, cursor, MaxSessionReadBytes, true)
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		cursor = part.Cursor
	}
	terminalWrite(t, session, "printf '\\344'; sleep 0.05; printf '\\270\\255'; printf '\\377'; printf 'fin%s' 'ished'\r")
	output, _ := terminalReadUntil(t, session, cursor, "finished")
	if !strings.Contains(output, "中�finished") {
		t.Fatal("跨读取边界的 UTF-8 或无效字节没有正确处理")
	}
}

func TestTerminalBlockedInputRespectsCancellation(t *testing.T) {
	_, session := terminalFixture(t)
	terminalWrite(t, session, "stty -echo -icanon; printf 'rea%s' 'dy'; sleep 30\r")
	terminalReadUntil(t, session, 0, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := session.Write(ctx, strings.Repeat("x", MaxSessionInputBytes))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal("终端输入阻塞没有按期取消", err)
	}
}
