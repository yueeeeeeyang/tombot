package computertools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type terminalStub struct {
	starts, reads int
	start         func(context.Context, computer.TerminalStart) (computer.TerminalOutput, error)
	read          func(context.Context, computer.TerminalRead) (computer.TerminalOutput, error)
}

func (runtime *terminalStub) StartTerminal(ctx context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
	runtime.starts++
	return runtime.start(ctx, request)
}
func (runtime *terminalStub) ReadTerminal(ctx context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
	runtime.reads++
	return runtime.read(ctx, request)
}
func (*terminalStub) WriteTerminal(context.Context, computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return computer.TerminalReceipt{}, errors.New("非交互 Shell 不应写入终端")
}
func (*terminalStub) ResizeTerminal(context.Context, computer.TerminalResize) error {
	return errors.New("非交互 Shell 不应修改终端尺寸")
}

func shellAddress() computer.TerminalAddress {
	return computer.TerminalAddress{OperationID: ids.New(), Session: computer.ExecutionSession{
		DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1},
		NodeID:          ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1,
	}}
}

func shellOptions() ShellOptions {
	return ShellOptions{CallTimeout: 100 * time.Millisecond, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 1024}
}

func newShell(t *testing.T, runtime computer.TerminalRuntime) *Shell {
	t.Helper()
	tool, err := NewShell(runtime, shellOptions())
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestShellInputAndPolicyRejection(t *testing.T) {
	for _, arguments := range []string{
		`null`, `[]`, `{}`, `{"command":null}`, `{"command":""}`, `{"command":"   "}`, `{"Command":"pwd"}`,
		`{"command":"pwd","command":"ls"}`, `{"command":"pwd"} {}`, `{"command":"pwd","extra":true}`,
		`{"command":"pwd","cwd":null}`, `{"command":"pwd","cwd":""}`, `{"command":"pwd","cwd":"a\nb"}`,
		`{"command":"pwd","cwd":"a\\b"}`, `{"command":"pwd","timeout_seconds":null}`, `{"command":"pwd","timeout_seconds":0}`,
		`{"command":"pwd","timeout_seconds":601}`, `{"command":"pwd","timeout_seconds":1.5}`, `{"command":"pwd","timeout_seconds":"1"}`,
		`{"command":"p\u0000wd"}`, `{"command":"pwd","env_secrets":[]}`, `{"command":"` + strings.Repeat("x", 4097) + `"}`,
		`{"command":"pwd","cwd":"` + strings.Repeat("a", 4097) + `"}`, `{"command":"sudo id"}`, "{\"command\":\"\xff\"}",
	} {
		runtime := &terminalStub{}
		result, err := newShell(t, runtime).Execute(context.Background(), shellAddress(), arguments, "standard")
		if err != nil || !result.IsError || result.Content == "" || runtime.starts != 0 || runtime.reads != 0 {
			t.Fatalf("无效参数或策略拒绝产生外部操作：%q %+v %v", arguments, result, err)
		}
	}
}

func TestShellPreservesDesktopAndCommand(t *testing.T) {
	for _, test := range []struct{ arguments, cwd, command, mode string }{
		{`{"command":"printf '中文'"}`, "/workspace", "printf '中文'", "standard"},
		{`{"command":"pwd","cwd":"projects/../shared","timeout_seconds":600}`, "/workspace/shared", "pwd", "strict"},
		{`{"command":"pwd","cwd":"../tmp"}`, "/tmp", "pwd", "loose"},
		{`{"command":"pwd","cwd":"/usr/bin"}`, "/usr/bin", "pwd", "standard"},
	} {
		address := shellAddress()
		runtime := &terminalStub{start: func(ctx context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) > 110*time.Millisecond || request.TerminalAddress != address || request.Validate() != nil || request.Command.Program != "/bin/bash" || strings.Join(request.Command.Arguments, "|") != "--noprofile|--norc|-c|"+test.command || request.Command.WorkingDirectory != test.cwd || len(request.Command.Environment) != 0 {
				t.Fatal("命令、桌面授权或调用期限错误", request)
			}
			return computer.TerminalOutput{Text: "首段", Cursor: 6, ExitCode: -1}, nil
		}, read: func(ctx context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
			if request.TerminalAddress != address || request.Cursor != 6 || request.Validate() != nil {
				t.Fatal("读取游标或归属错误", request)
			}
			return computer.TerminalOutput{Text: "尾段", Cursor: 12, Exited: true, ExitCode: 7}, nil
		}}
		result, err := newShell(t, runtime).Execute(context.Background(), address, test.arguments, test.mode)
		if err != nil || !result.IsError || runtime.starts != 1 || runtime.reads != 1 || !strings.Contains(result.Content, `"output":"首段尾段"`) || !strings.Contains(result.Content, `"exit_code":7`) {
			t.Fatal("命令结果或执行次数错误", result, err)
		}
	}
}

func TestShellDrainsExitedOutputAndReportsTruncation(t *testing.T) {
	for _, output := range []string{strings.Repeat("中文", 3000), strings.Repeat("\x00\"\\", 3000)} {
		runtime := &terminalStub{start: func(context.Context, computer.TerminalStart) (computer.TerminalOutput, error) {
			return computer.TerminalOutput{Text: output, Cursor: int64(len(output) + 200), Truncated: true, Exited: true, HasMore: true, ExitCode: 0}, nil
		}, read: func(_ context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
			if request.Cursor != int64(len(output)+200) {
				t.Fatal("读取遗漏截断游标")
			}
			return computer.TerminalOutput{Text: "最后结果", Cursor: request.Cursor + 12, Exited: true, ExitCode: 0}, nil
		}}
		result, err := newShell(t, runtime).Execute(context.Background(), shellAddress(), `{"command":"pwd"}`, "standard")
		var payload struct {
			Output    string `json:"output"`
			ExitCode  int    `json:"exit_code"`
			Truncated bool   `json:"truncated"`
		}
		if err != nil || result.IsError || len(result.Content) > 1024 || json.Unmarshal([]byte(result.Content), &payload) != nil || !utf8.ValidString(payload.Output) || !strings.HasSuffix(payload.Output, "最后结果") || !payload.Truncated || payload.ExitCode != 0 || runtime.reads != 1 {
			t.Fatal("有界输出丢失尾部、退出码或截断状态", result, err)
		}
	}
}

func TestShellUnknownResultsNeverRepeat(t *testing.T) {
	for _, mode := range []string{"启动回包丢失", "读取回包丢失", "调用超时", "倒退游标", "虚假输出缺口", "输出读取失败", "退出等待失败", "无进度尾部", "无效退出码", "无效编码"} {
		t.Run(mode, func(t *testing.T) {
			runtime := &terminalStub{start: func(ctx context.Context, _ computer.TerminalStart) (computer.TerminalOutput, error) {
				if mode == "启动回包丢失" {
					return computer.TerminalOutput{}, errors.New("连接中断")
				}
				if mode == "调用超时" {
					<-ctx.Done()
					return computer.TerminalOutput{}, ctx.Err()
				}
				return computer.TerminalOutput{Text: "ok", Cursor: 2, ExitCode: -1}, nil
			}, read: func(context.Context, computer.TerminalRead) (computer.TerminalOutput, error) {
				output := computer.TerminalOutput{Cursor: 2, Exited: true, ExitCode: 0}
				switch mode {
				case "读取回包丢失":
					return output, errors.New("连接中断")
				case "倒退游标":
					output.Cursor = 1
				case "虚假输出缺口":
					output.Cursor = 3
				case "输出读取失败":
					output.ErrorCode = "terminal_read_failed"
				case "退出等待失败":
					output.ErrorCode = "terminal_wait_failed"
				case "无进度尾部":
					output.HasMore = true
				case "无效退出码":
					output.ExitCode = 256
				case "无效编码":
					output.Text = "\xff"
					output.Cursor = 3
				}
				return output, nil
			}}
			result, err := newShell(t, runtime).Execute(context.Background(), shellAddress(), `{"command":"pwd"}`, "standard")
			if err == nil || result.Content != "" || runtime.starts != 1 || runtime.reads > 1 {
				t.Fatal("未知操作被重发或伪装成功", result, err, runtime.starts, runtime.reads)
			}
		})
	}
}

func TestShellCancellationAndDeadline(t *testing.T) {
	for _, cancelBefore := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelBefore {
			cancel()
		}
		runtime := &terminalStub{start: func(context.Context, computer.TerminalStart) (computer.TerminalOutput, error) {
			cancel()
			return computer.TerminalOutput{ExitCode: -1}, nil
		}}
		_, err := newShell(t, runtime).Execute(ctx, shellAddress(), `{"command":"pwd"}`, "standard")
		cancel()
		if !errors.Is(err, context.Canceled) || runtime.reads != 0 || cancelBefore && runtime.starts != 0 {
			t.Fatal("取消后继续执行", err)
		}
	}
	runtime := &terminalStub{start: func(context.Context, computer.TerminalStart) (computer.TerminalOutput, error) {
		return computer.TerminalOutput{ExitCode: -1}, nil
	}, read: func(context.Context, computer.TerminalRead) (computer.TerminalOutput, error) {
		return computer.TerminalOutput{ExitCode: -1}, nil
	}}
	started := time.Now()
	_, err := newShell(t, runtime).Execute(context.Background(), shellAddress(), `{"command":"pwd","timeout_seconds":1}`, "standard")
	if !errors.Is(err, context.DeadlineExceeded) || runtime.starts != 1 || runtime.reads == 0 || time.Since(started) > 2*time.Second {
		t.Fatal("命令总期限未生效", err)
	}
}

func TestShellRejectsInvalidAuthorityAndConfiguration(t *testing.T) {
	runtime := &terminalStub{}
	tool := newShell(t, runtime)
	address := shellAddress()
	for _, invalid := range []computer.TerminalAddress{{}, {Session: address.Session, OperationID: "bad"}} {
		if _, err := tool.Execute(context.Background(), invalid, `{"command":"pwd"}`, "standard"); err == nil {
			t.Fatal("接受无效执行授权")
		}
	}
	if _, err := tool.Execute(context.Background(), address, `{"command":"pwd"}`, "unknown"); err == nil {
		t.Fatal("接受无效策略")
	}
	if runtime.starts != 0 {
		t.Fatal("无效授权发起命令")
	}
	if _, err := NewShell(nil, shellOptions()); err == nil {
		t.Fatal("允许缺失运行器")
	}
	for _, change := range []func(*ShellOptions){func(o *ShellOptions) { o.CallTimeout = 0 }, func(o *ShellOptions) { o.CallTimeout = 11 * time.Second }, func(o *ShellOptions) { o.PollInterval = 0 }, func(o *ShellOptions) { o.PollInterval = 2 * time.Second }, func(o *ShellOptions) { o.MaximumResultBytes = 1023 }, func(o *ShellOptions) { o.MaximumResultBytes = 25 << 10 }} {
		options := shellOptions()
		change(&options)
		if _, err := NewShell(runtime, options); err == nil {
			t.Fatal("接受无界配置", options)
		}
	}
}
