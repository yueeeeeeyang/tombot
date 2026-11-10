package term

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
)

func shellHome(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.MkdirAll(filepath.Join(path, ".tom", "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestShellOutputExitEnvironmentAndRedaction(t *testing.T) {
	path := shellHome(t)
	hidden := testkit.Random(t)
	t.Setenv("TOM_TEST_PRIVATE", hidden)
	injected := []byte(testkit.Random(t))
	defer clear(injected)
	result, err := NewShell().Run(context.Background(), ShellInput{Command: `printf '%s' "$TASK_VALUE"; printf stderr >&2; printf '%s' "${TOM_TEST_PRIVATE-unset}"; exit 7`}, path, path, map[string][]byte{"TASK_VALUE": injected})
	if err != nil || result.ExitCode != 7 || result.ErrorCode != "SHELL_EXIT_FAILED" || !strings.Contains(result.StdoutTail, "***stderrunset") || strings.Contains(string(result.Output), string(injected)) || strings.Contains(string(result.Output), hidden) {
		t.Fatal("命令状态或脱敏错误", err, result.ExitCode, result.ErrorCode)
	}
}
func TestCaptureRedactsAtEveryChunkBoundary(t *testing.T) {
	value := []byte(testkit.Random(t))
	defer clear(value)
	content := append([]byte("before:"), value...)
	content = append(content, []byte(":after")...)
	for split := 1; split < len(content); split++ {
		stopped := false
		output := newCapture(map[string][]byte{"TASK": value}, 1024, func() { stopped = true })
		if _, err := output.Write(content[:split]); err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(content[split:]); err != nil {
			t.Fatal(err)
		}
		clean, overflow, err := output.finish()
		if string(clean) != "before:***:after" || overflow || stopped || err != nil {
			t.Fatal("输出分片脱敏失败", split)
		}
	}
}
func TestShellTimeoutKillsIgnoringChildGroup(t *testing.T) {
	path := shellHome(t)
	shell := NewShell()
	shell.terminationGrace = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := shell.Run(ctx, ShellInput{Command: `trap '' TERM; (trap '' TERM; while :; do sleep 1; done) & printf '%s' "$!"; wait`}, path, path, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatal("超时没有及时回收", err)
	}
	pid, parseErr := strconv.Atoi(result.StdoutTail)
	if parseErr != nil {
		t.Fatal("缺少子进程标识", parseErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("命令子进程仍然存活", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestShellTruncationOutputLimitAndValidation(t *testing.T) {
	path := shellHome(t)
	shell := NewShell()
	shell.maximumOutput = TailBytes + 1024
	shell.terminationGrace = 50 * time.Millisecond
	result, err := shell.Run(context.Background(), ShellInput{Command: `while :; do printf 'output line\n'; done`}, path, path, nil)
	if err != nil || result.ErrorCode != "TOOL_OUTPUT_LIMIT" || !result.Truncated || len(result.Output) != shell.maximumOutput || len(result.StdoutTail) > TailBytes {
		t.Fatal("输出上限未生效", err, result.ErrorCode, len(result.Output))
	}
	for _, input := range []ShellInput{{Command: ""}, {Command: "true", TimeoutSeconds: -1}, {Command: "true", TimeoutSeconds: 601}, {Command: "true", EnvSecrets: []EnvironmentSecret{{Name: "PATH", SecretID: "reference"}}}, {Command: "true", EnvSecrets: []EnvironmentSecret{{Name: "TASK", SecretID: "reference"}, {Name: "TASK", SecretID: "another"}}}} {
		if ValidateShell(input) == nil {
			t.Fatal("无效参数被接受")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = shell.Run(canceled, ShellInput{Command: "true"}, path, path, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消请求仍执行", err)
	}
}

func TestShellBinaryOutputAndExplicitCancellation(t *testing.T) {
	home := shellHome(t)
	result, err := NewShell().Run(context.Background(), ShellInput{Command: `printf '\000\377'`}, home, home, nil)
	if err != nil || !result.HasBinaryOutput || !result.Truncated || string(result.Output) != string([]byte{0, 255}) {
		t.Fatal("二进制输出缺少标注", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	result, err = NewShell().Run(ctx, ShellInput{Command: `printf started; sleep 30`}, home, home, nil)
	if !errors.Is(err, context.Canceled) || result.ErrorCode != "TOOL_CANCELED" || result.StdoutTail != "started" {
		t.Fatal("取消未保留输出", err, result.ErrorCode)
	}
}
