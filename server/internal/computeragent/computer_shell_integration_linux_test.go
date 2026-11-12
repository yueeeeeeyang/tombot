//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/terminalio"
)

// localShellRuntime 仅在临时测试容器中直接降权启动 PTY，验证真实程序和工具桥接，不替代生产 cgroup 验收。
type localShellRuntime struct {
	t        *testing.T
	session  computer.ExecutionSession
	address  computer.TerminalAddress
	terminal *managedTerminal
}

func (runtime *localShellRuntime) StartTerminal(ctx context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
	if request.Validate() != nil || request.Session != runtime.session || runtime.terminal != nil {
		return computer.TerminalOutput{}, errors.New("测试终端授权或启动次数错误")
	}
	master, slave, err := pty.Open()
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	defer func() {
		if err := slave.Close(); err != nil {
			runtime.t.Error(err)
		}
	}()
	if err := slave.Chown(request.Session.UID, request.Session.GID); err != nil {
		return computer.TerminalOutput{}, errors.Join(err, master.Close())
	}
	if err := slave.Chmod(0600); err != nil {
		return computer.TerminalOutput{}, errors.Join(err, master.Close())
	}
	if err := terminalio.ResizePTY(master, request.Columns, request.Rows); err != nil {
		return computer.TerminalOutput{}, errors.Join(err, master.Close())
	}
	master, err = terminalio.PollablePTY(master)
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	environment, err := request.Session.DesktopEnvironment()
	if err != nil {
		return computer.TerminalOutput{}, errors.Join(err, master.Close())
	}
	command := exec.Command(request.Command.Program, request.Command.Arguments...)
	command.Dir = request.Command.WorkingDirectory
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Credential: &syscall.Credential{Uid: uint32(request.Session.UID), Gid: uint32(request.Session.GID)}}
	if err = ctx.Err(); err == nil {
		err = command.Start()
	}
	if err != nil {
		return computer.TerminalOutput{}, errors.Join(err, master.Close())
	}
	terminal := &managedTerminal{master: master, command: command, output: terminalio.NewBuffer(terminalBufferBytes), exitCode: -1, done: make(chan struct{}), readDone: make(chan struct{})}
	runtime.address, runtime.terminal = request.TerminalAddress, terminal
	go terminal.readOutput()
	go terminal.wait()
	runtime.t.Cleanup(func() {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			runtime.t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := terminal.closeAfterDrain(ctx); err != nil {
			runtime.t.Error(err)
		}
	})
	return terminal.read(0, computer.MaximumTerminalReadBytes)
}
func (runtime *localShellRuntime) ReadTerminal(_ context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
	if request.Validate() != nil || request.TerminalAddress != runtime.address || runtime.terminal == nil {
		return computer.TerminalOutput{}, errors.New("终端读取归属错误")
	}
	return runtime.terminal.read(request.Cursor, request.MaximumBytes)
}
func (*localShellRuntime) WriteTerminal(context.Context, computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return computer.TerminalReceipt{}, errors.New("非交互测试不接受输入")
}
func (*localShellRuntime) ResizeTerminal(context.Context, computer.TerminalResize) error {
	return errors.New("非交互测试不接受尺寸变更")
}

type shellAgentRelay struct {
	tool                *execution.ComputerShell
	session             computer.ExecutionSession
	arguments, expected string
	models, tools       int
}

func (relay *shellAgentRelay) Model(_ context.Context, key string, input modelgateway.Input) (botagent.ModelOutput, error) {
	relay.models++
	if key == "model.1" {
		return botagent.ModelOutput{StopReason: "tool_calls", Tools: []botagent.ToolCall{{ID: "shared-shell", Name: "computer_shell", Arguments: relay.arguments}}}, nil
	}
	if key != "model.2" || len(input.Messages) != 3 || len(input.Messages[2].Blocks) != 1 || input.Messages[2].Blocks[0].ToolResult == nil {
		return botagent.ModelOutput{}, errors.New("真实 Agent 未收到 Shell 结果")
	}
	result := input.Messages[2].Blocks[0].ToolResult
	var output struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if result.IsError || json.Unmarshal([]byte(result.Content), &output) != nil || output.Output != relay.expected || output.ExitCode != 0 {
		return botagent.ModelOutput{}, errors.New("真实 Shell 的身份、文件共享或独立 DISPLAY 不符")
	}
	return botagent.ModelOutput{Text: "已从共享电脑完成命令。", StopReason: "stop"}, nil
}
func (relay *shellAgentRelay) Tool(ctx context.Context, key string, call botagent.ToolCall) (botagent.ToolResult, error) {
	relay.tools++
	if key != "tool.1.1" {
		return botagent.ToolResult{}, errors.New("工具次数不符")
	}
	return relay.tool.Execute(ctx, execution.Invocation{Computer: &relay.session, OperationID: ids.New(), ApprovalMode: "standard", Call: call, Lease: execution.Lease{UserID: relay.session.UserID, BotID: relay.session.BotID, RunID: relay.session.RunID}})
}

func TestKernelContainerAgentShellAndSharedFiles(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("真实 Agent 与 Shell 测试必须运行在专用镜像")
	}
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	shared, err := os.MkdirTemp("/workspace", "shell-agents-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(shared); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chown(shared, 0, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	_, other := desktopCommand(fixture.command, 2)
	agents := []scopedAgentRuntime{testManagedBot(t, fixture.reference), testManagedBot(t, other)}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 2)
	for index, agent := range agents {
		display, peer := strconv.Itoa(index+1), strconv.Itoa(2-index)
		// 两个命令互相等候共享文件，只有真实并行执行且公共组可读写才能完成。
		script := `set -eu; umask 0007; printf '%s' "$(id -u)" > bot-` + display + `; while [ ! -s bot-` + peer + ` ]; do sleep 0.01; done; cat bot-` + peer + `; printf '|%s' "$DISPLAY"`
		arguments, err := json.Marshal(map[string]any{"command": script, "cwd": shared, "timeout_seconds": 5})
		if err != nil {
			t.Fatal(err)
		}
		runtime := &localShellRuntime{t: t, session: agent.reference.Session}
		tool, err := execution.NewComputerShell(runtime, computertools.ShellOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		relay := &shellAgentRelay{tool: tool, session: agent.reference.Session, arguments: string(arguments), expected: strconv.Itoa(computer.DesktopUIDBase+2-index) + "|:" + display}
		start := botagent.Start{Input: modelgateway.Input{System: "Use the remote computer to complete the task.", Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Verify shared files and your desktop identity."}}}}, Tools: []provider.ToolSchema{tool.Schema()}}, Limits: botagent.Limits{ModelSteps: 3, ToolCalls: 2, OutputBytes: 4096, ContextBytes: 32 << 10}}
		go func() {
			answer, err := botagent.Dispatch(ctx, agent, agent.reference.Session, start, relay, time.Second)
			if err == nil && (answer != "已从共享电脑完成命令。" || relay.models != 2 || relay.tools != 1) {
				err = errors.New("Agent 与 Shell 未完成完整循环")
			}
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	for _, name := range []string{"bot-1", "bot-2"} {
		content, err := os.ReadFile(shared + "/" + name)
		if err != nil || !strings.HasPrefix(string(content), "10000") {
			t.Fatal("共享文件未由普通 Bot 身份生成", err)
		}
	}
}
