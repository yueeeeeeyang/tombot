package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computertools"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type shellNode struct {
	*sessionNode
	start  func(context.Context, computer.TerminalStart) (computer.TerminalOutput, error)
	starts int
}

func (node *shellNode) StartTerminal(ctx context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
	node.starts++
	node.mu.Lock()
	state := node.states[request.Session.DesktopID]
	node.mu.Unlock()
	if state.State != "active" || state.Session != request.Session {
		return computer.TerminalOutput{}, errors.New("缺少当前桌面执行权")
	}
	return node.start(ctx, request)
}
func (*shellNode) ReadTerminal(context.Context, computer.TerminalRead) (computer.TerminalOutput, error) {
	return computer.TerminalOutput{}, errors.New("测试命令已结束")
}
func (*shellNode) WriteTerminal(context.Context, computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return computer.TerminalReceipt{}, errors.New("Shell 不应发送交互输入")
}
func (*shellNode) ResizeTerminal(context.Context, computer.TerminalResize) error {
	return errors.New("Shell 不应修改尺寸")
}

func TestWorkerComputerShellLedgerAndUnknownOutcome(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"成功", "非零退出", "策略拒绝", "启动回包丢失"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			fixture.toolsModel(t)
			spec := fixture.first.Spec
			spec.ToolPermissions = []string{"computer_shell"}
			bot, err := fixture.catalog.Update(context.Background(), fixture.a, fixture.first.ID, bots.UpdateRequest{ExpectedRevision: fixture.first.Revision, Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			fixture.send(t, fixture.a, bot)
			lease := fixture.claim(t)
			node := &shellNode{sessionNode: &sessionNode{}}
			node.start = func(_ context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
				var kind, status, sessionID string
				if err := fixture.pool.QueryRow(`SELECT kind,status,computer_session_id FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, request.OperationID).Scan(&kind, &status, &sessionID); err != nil || kind != "computer" || status != "started" || sessionID != request.Session.SessionID || request.Session.UserID != lease.UserID || request.Session.BotID != lease.BotID || request.Session.RunID != lease.RunID {
					t.Fatal("外部命令执行前未绑定持久操作与桌面", kind, status, sessionID, err)
				}
				if mode == "启动回包丢失" {
					return computer.TerminalOutput{}, errors.New("启动后响应中断")
				}
				exitCode := 0
				if mode == "非零退出" {
					exitCode = 9
				}
				return computer.TerminalOutput{Text: "容器输出", Cursor: 12, Exited: true, ExitCode: exitCode}, nil
			}
			tool, err := NewComputerShell(node, computertools.ShellOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 8 << 10})
			if err != nil {
				t.Fatal(err)
			}
			models := 0
			worker := fixture.workerWithComputers(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				models++
				if models == 1 {
					arguments := `{"command":"printf ready"}`
					if mode == "策略拒绝" {
						arguments = `{"command":"sudo id"}`
					}
					for _, event := range []provider.Event{{Kind: "tool_use_start", Index: 0, ToolCallID: "shell-call", ToolName: "computer_shell"}, {Kind: "tool_use_delta", Index: 0, ToolCallID: "shell-call", InputDelta: arguments}, {Kind: "tool_use_end", Index: 0, ToolCallID: "shell-call", ToolName: "computer_shell", InputDelta: arguments}, {Kind: "stop", StopReason: "tool_calls"}} {
						if err := emit(event); err != nil {
							return modelgateway.Result{}, err
						}
					}
					return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
				}
				if models != 2 || mode == "启动回包丢失" {
					t.Error("未知动作后仍继续模型调用")
				}
				blocks := input.Messages[len(input.Messages)-1].Blocks
				if len(blocks) != 1 || blocks[0].ToolResult == nil || blocks[0].ToolResult.IsError != (mode != "成功") {
					t.Fatal("工具结果未传回容器 Agent", blocks)
				}
				return emitText(emit, "已处理命令结果。")
			}), fixture.computers(t, node, computerOptions()), tool)
			run, err := worker.Process(context.Background(), lease)
			expectedStatus, expectedOperation := "succeeded", "succeeded"
			if mode == "启动回包丢失" {
				expectedStatus, expectedOperation = "unknown", "unknown"
			} else if mode != "成功" {
				expectedOperation = "failed"
			}
			if err != nil || run.Status != expectedStatus || fixture.sessionState(t, lease) != "released" {
				t.Fatal("运行结果或清理状态错误", run, err)
			}
			var operationStatus string
			if err = fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND kind='computer'`, lease.UserID, lease.RunID).Scan(&operationStatus); err != nil || operationStatus != expectedOperation {
				t.Fatal("工具账本状态错误", operationStatus, err)
			}
			expectedStarts := 1
			if mode == "策略拒绝" {
				expectedStarts = 0
			}
			if node.starts != expectedStarts || node.closes != 1 {
				t.Fatal("命令重发或未定向清理", node.starts, node.closes)
			}
			if mode == "启动回包丢失" && (models != 1 || run.ErrorCode != "EXTERNAL_RESULT_UNKNOWN") {
				t.Fatal("未知动作没有停止运行", models, run)
			}
		})
	}
}

func TestComputerShellRejectsForeignInvocation(t *testing.T) {
	node := &shellNode{sessionNode: &sessionNode{}}
	tool, err := NewComputerShell(node, computertools.ShellOptions{CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, MaximumResultBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	session := computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	for _, change := range []func(*Invocation){
		func(i *Invocation) { i.Computer = nil }, func(i *Invocation) { i.Lease.UserID = ids.New() }, func(i *Invocation) { i.Lease.BotID = ids.New() }, func(i *Invocation) { i.Lease.RunID = ids.New() }, func(i *Invocation) { i.Call.Name = "file_read" },
	} {
		invocation := Invocation{Computer: &session, OperationID: ids.New(), ApprovalMode: "standard", Call: botagent.ToolCall{Name: "computer_shell", Arguments: `{"command":"pwd"}`}, Lease: Lease{UserID: session.UserID, BotID: session.BotID, RunID: session.RunID}}
		change(&invocation)
		if _, err = tool.Execute(context.Background(), invocation); !errors.Is(err, ErrComputerLost) {
			t.Fatal("接受跨用户、Bot 或运行调用", err)
		}
	}
	if node.starts != 0 {
		t.Fatal("归属拒绝仍执行命令")
	}
	if !tool.UsesComputer() || !json.Valid(tool.Schema().InputSchema) {
		t.Fatal("工具声明未绑定电脑")
	}
}
