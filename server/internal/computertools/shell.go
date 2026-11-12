// Package computertools 将模型工具转换为当前用户电脑内的受管操作，不执行宿主程序。
package computertools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/terminalio"
)

type ShellOptions struct {
	CallTimeout        time.Duration
	PollInterval       time.Duration
	MaximumResultBytes int
}

type Shell struct {
	runtime computer.TerminalRuntime
	options ShellOptions
}

// NewShell 要求节点提供真实终端能力及显式上限；结果上限须不大于 Worker 的工具输出上限。
func NewShell(runtime computer.TerminalRuntime, options ShellOptions) (*Shell, error) {
	if runtime == nil || options.CallTimeout < 100*time.Millisecond || options.CallTimeout > 10*time.Second || options.PollInterval < 10*time.Millisecond || options.PollInterval > time.Second || options.MaximumResultBytes < 1024 || options.MaximumResultBytes > 24<<10 {
		return nil, errors.New("电脑 Shell 工具配置无效")
	}
	return &Shell{runtime: runtime, options: options}, nil
}

func (tool *Shell) Schema() provider.ToolSchema {
	return provider.ToolSchema{
		Name:        "computer_shell",
		Description: "Run one non-interactive shell command in your user's shared remote computer as your bot's non-root identity. All bots of this user share business files and installed tools. The default directory is /workspace; relative cwd is resolved from /workspace, and other accessible absolute container paths are allowed. Timeout defaults to 120 seconds, maximum 600, also bounded by the run deadline. Output combines stdout and stderr from a PTY and retains a bounded tail. A timeout or transport failure stops the run because command effects may be unknown. A completed shell does not imply its background descendants have exited; remaining run processes are cleaned up when the run ends. Privileged commands are denied. Never place secret values in commands.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1,"maxLength":4096},"cwd":{"type":"string","minLength":1,"maxLength":4096},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}},"required":["command"],"additionalProperties":false}`),
	}
}

type shellInput struct {
	command string
	cwd     string
	timeout time.Duration
}

// decodeShell 使用逐字段解码拒绝重复键、大小写别名和 null，避免审计摘要与实际命令含义不一致。
func decodeShell(arguments string) (shellInput, bool) {
	input := shellInput{cwd: "/workspace", timeout: 120 * time.Second}
	if len(arguments) > 32<<10 || !utf8.ValidString(arguments) {
		return input, false
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return input, false
	}
	seen := map[string]bool{}
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || seen[name] {
			return input, false
		}
		seen[name] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || string(raw) == "null" {
			return input, false
		}
		switch name {
		case "command":
			if json.Unmarshal(raw, &input.command) != nil || strings.TrimSpace(input.command) == "" || len(input.command) > 4096 || strings.ContainsRune(input.command, 0) {
				return input, false
			}
		case "cwd":
			if json.Unmarshal(raw, &input.cwd) != nil || input.cwd == "" || len(input.cwd) > 4096 || strings.ContainsAny(input.cwd, "\x00\\\r\n") {
				return input, false
			}
		case "timeout_seconds":
			var seconds int
			if json.Unmarshal(raw, &seconds) != nil || seconds < 1 || seconds > 600 {
				return input, false
			}
			input.timeout = time.Duration(seconds) * time.Second
		default:
			return input, false
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') || !seen["command"] {
		return input, false
	}
	if decoder.Decode(new(any)) != io.EOF {
		return input, false
	}
	if !path.IsAbs(input.cwd) {
		input.cwd = path.Join("/workspace", input.cwd)
	} else {
		input.cwd = path.Clean(input.cwd)
	}
	return input, true
}

// Execute 以已记账的操作标识启动一次命令并按游标读取；不重发启动，不把超时当作已知退出。
// 普通参数、策略拒绝和已知非零退出回灌模型；授权、协议和通信失败交给 Worker 终止并回收运行。
func (tool *Shell) Execute(ctx context.Context, address computer.TerminalAddress, arguments, approvalMode string) (botagent.ToolResult, error) {
	if address.Validate() != nil {
		return botagent.ToolResult{}, errors.New("电脑 Shell 执行身份无效")
	}
	if err := ctx.Err(); err != nil {
		return botagent.ToolResult{}, err
	}
	input, valid := decodeShell(arguments)
	if !valid {
		return botagent.ToolResult{IsError: true, Content: "Invalid shell arguments. Supply command, optional cwd, and optional timeout_seconds between 1 and 600."}, nil
	}
	needsApproval, err := policy.NeedsApproval(approvalMode, "medium")
	if err != nil {
		return botagent.ToolResult{}, errors.New("电脑 Shell 策略无效")
	}
	if needsApproval || policy.ShellClass(input.command) == "shell.privileged" {
		return botagent.ToolResult{IsError: true, Content: "A policy rule denied this shell command."}, nil
	}
	request := computer.TerminalStart{TerminalAddress: address, Command: computer.ProcessCommand{WorkingDirectory: input.cwd, Program: "/bin/bash", Arguments: []string{"--noprofile", "--norc", "-c", input.command}}, Columns: 100, Rows: 30}
	if request.Validate() != nil {
		return botagent.ToolResult{IsError: true, Content: "The shell command or working directory is invalid."}, nil
	}
	bounded, cancel := context.WithTimeout(ctx, input.timeout)
	defer cancel()
	call, stop := context.WithTimeout(bounded, tool.options.CallTimeout)
	output, err := tool.runtime.StartTerminal(call, request)
	callErr := call.Err()
	stop()
	if err != nil || callErr != nil {
		return botagent.ToolResult{}, errors.Join(errors.New("电脑 Shell 启动结果未确认"), callErr)
	}
	tail := terminalio.NewBuffer(tool.options.MaximumResultBytes)
	var cursor int64
	truncated := false
	for {
		if err := bounded.Err(); err != nil {
			return botagent.ToolResult{}, err
		}
		if output.Validate(cursor, computer.MaximumTerminalReadBytes) != nil || output.ErrorCode != "" {
			return botagent.ToolResult{}, errors.New("电脑 Shell 输出或退出状态未确认")
		}
		tail.Append([]byte(output.Text))
		cursor, truncated = output.Cursor, truncated || output.Truncated
		if output.Exited && !output.HasMore {
			return tool.result(&tail, output.ExitCode, truncated)
		}
		if !output.HasMore {
			timer := time.NewTimer(tool.options.PollInterval)
			select {
			case <-bounded.Done():
				timer.Stop()
				return botagent.ToolResult{}, bounded.Err()
			case <-timer.C:
			}
		}
		call, stop := context.WithTimeout(bounded, tool.options.CallTimeout)
		output, err = tool.runtime.ReadTerminal(call, computer.TerminalRead{TerminalAddress: address, Cursor: cursor, MaximumBytes: computer.MaximumTerminalReadBytes})
		callErr := call.Err()
		stop()
		if err != nil || callErr != nil {
			return botagent.ToolResult{}, errors.Join(errors.New("电脑 Shell 读取结果未确认"), callErr)
		}
	}
}

// result 的容量包含 JSON 转义，截断只丢弃输出前部，保留 UTF-8 边界和真实退出码。
func (tool *Shell) result(tail *terminalio.Buffer, exitCode int, truncated bool) (botagent.ToolResult, error) {
	output, _, clipped := tail.Read(0, tool.options.MaximumResultBytes)
	result := struct {
		Output    string `json:"output"`
		ExitCode  int    `json:"exit_code"`
		Truncated bool   `json:"truncated"`
	}{output, exitCode, truncated || clipped}
	for {
		encoded, err := json.Marshal(result)
		if err != nil {
			return botagent.ToolResult{}, errors.New("电脑 Shell 结果编码失败")
		}
		if len(encoded) <= tool.options.MaximumResultBytes {
			return botagent.ToolResult{Content: string(encoded), IsError: exitCode != 0}, nil
		}
		// 单个控制字节会编码为六字节；不能按编码超额直接删除原文，否则会连有效尾部一起丢弃。
		drop := min(len(result.Output), max(1, (len(encoded)-tool.options.MaximumResultBytes+5)/6))
		for drop < len(result.Output) && !utf8.RuneStart(result.Output[drop]) {
			drop++
		}
		result.Output, result.Truncated = result.Output[drop:], true
	}
}
