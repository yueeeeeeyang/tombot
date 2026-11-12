//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

// nativeBrowserRelay 只替代模型与持久账本；实际 Agent 进程、本地工具、Unix 入口和 Chrome 均使用正式实现。
type nativeBrowserRelay struct {
	session         computer.ExecutionSession
	url, window     string
	step, completed int
	pending         botagent.ToolGrant
	imageReceived   bool
	uploadPaths     []string
	afterUpload     func(string) error
}

func (relay *nativeBrowserRelay) Model(_ context.Context, _ string, input modelgateway.Input) (botagent.ModelOutput, error) {
	relay.step++
	arguments := map[string]any{"window_id": relay.window}
	step := relay.step
	if len(relay.uploadPaths) > 0 && step == 4 {
		arguments["action"], arguments["selector"], arguments["paths"] = "upload", "#files", relay.uploadPaths
	} else {
		if len(relay.uploadPaths) > 0 && step > 4 {
			step--
		}
		switch step {
		case 1:
			arguments = map[string]any{"action": "open"}
		case 2:
			arguments["action"], arguments["url"] = "navigate", relay.url
		case 3:
			arguments["action"] = "screenshot"
		case 4:
			arguments["action"] = "inspect"
		case 5:
			result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
			var observed struct {
				Window Window `json:"window"`
			}
			if result == nil || json.Unmarshal([]byte(result.Content), &observed) != nil || observed.Window.Title != "shared" {
				return botagent.ModelOutput{}, errors.New("Agent 未读取共享账号页面")
			}
			arguments["action"] = "close"
		case 6:
			return botagent.ModelOutput{Text: "浏览器操作已完成。", StopReason: "stop"}, nil
		default:
			return botagent.ModelOutput{}, errors.New("Agent 产生额外模型步骤")
		}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return botagent.ModelOutput{}, err
	}
	return botagent.ModelOutput{StopReason: "tool_calls", Tools: []botagent.ToolCall{{ID: ids.New(), Name: "computer_browser", Arguments: string(encoded)}}}, nil
}
func (*nativeBrowserRelay) Tool(context.Context, string, botagent.ToolCall) (botagent.ToolResult, error) {
	return botagent.ToolResult{}, errors.New("浏览器动作错误地回到服务端执行")
}
func (relay *nativeBrowserRelay) PrepareLocal(_ context.Context, _ string, _ botagent.ToolCall) (botagent.ToolPreparation, error) {
	grant := botagent.ToolGrant{OperationID: ids.New(), Sequence: int64(relay.step), Session: relay.session, ApprovalMode: "standard", TimeoutMS: 10000, OutputBytes: 8192}
	if relay.step == 1 {
		relay.window = grant.OperationID
	}
	relay.pending = grant
	return botagent.ToolPreparation{Grant: &grant}, nil
}
func (relay *nativeBrowserRelay) CompleteLocal(_ context.Context, grant botagent.ToolGrant, report botagent.ToolReport) (botagent.ToolResult, error) {
	if grant != relay.pending || report.Validate(grant) != nil || report.Failure != "" || report.Result.IsError {
		return botagent.ToolResult{}, errors.New("真实 Agent 本地结果未确认")
	}
	relay.completed++
	if relay.step == 4 && len(relay.uploadPaths) > 0 && relay.afterUpload != nil {
		if err := relay.afterUpload(relay.window); err != nil {
			return botagent.ToolResult{}, err
		}
	}
	if ref := report.Result.Artifact; ref != nil {
		if botagent.ValidateScreenshotPixels(ref) != nil {
			return botagent.ToolResult{}, errors.New("真实 Agent 图片未完整传回")
		}
		relay.imageReceived = true
		ref.Content = nil
	}
	return report.Result, nil
}

func runNativeBrowserAgent(t *testing.T, session computer.ExecutionSession, url string) {
	runNativeBrowserAgentWithUpload(t, session, url, nil, nil)
}

func runNativeBrowserAgentWithUpload(t *testing.T, session computer.ExecutionSession, url string, paths []string, verify func(string) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fds[0]), "parent")
	child := os.NewFile(uintptr(fds[1]), "child")
	connection, err := net.FileConn(parent)
	closeError := parent.Close()
	if err != nil || closeError != nil {
		t.Fatal(errors.Join(err, closeError, child.Close()))
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	command := exec.CommandContext(ctx, "/usr/lib/tombot/bot-agent")
	command.Stdin, command.Stdout = child, child
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(session.UID), Gid: uint32(session.GID)}}
	err = command.Start()
	closeError = child.Close()
	if err != nil || closeError != nil {
		t.Fatal(errors.Join(err, closeError))
	}
	start := botagent.Start{Input: modelgateway.Input{System: "Use your local browser.", Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "打开网页并截图。"}}}}, Tools: []provider.ToolSchema{{Name: "computer_browser", Description: "Use your local browser.", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, LocalTools: []string{"computer_browser"}, Limits: botagent.Limits{ModelSteps: 8, ToolCalls: 1, OutputBytes: 8192, ContextBytes: 64 << 10}}
	relay := &nativeBrowserRelay{session: session, url: url, uploadPaths: paths, afterUpload: verify}
	answer, dispatchError := botagent.DispatchStream(ctx, connection, start, relay)
	waitError := command.Wait()
	expected := 5
	if len(paths) > 0 {
		expected++
	}
	if dispatchError != nil || waitError != nil || answer != "浏览器操作已完成。" || relay.completed != expected || !relay.imageReceived {
		t.Fatal("真实 Agent 本地浏览器调用失败", dispatchError, waitError, relay.completed, relay.imageReceived, diagnostics.String())
	}
}
