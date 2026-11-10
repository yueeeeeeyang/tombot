package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/term"
)

func openToolTerminal(t *testing.T, e *Executor, bot domain.Bot, run domain.Run) service.Terminal {
	t.Helper()
	result := execute(t, e, bot, run, "terminal_session", map[string]any{"action": "open"})
	var terminal service.Terminal
	if result.IsError {
		t.Fatal(result.Content)
	}
	if err := json.Unmarshal([]byte(result.Content), &terminal); err != nil || terminal.ID == "" {
		t.Fatal("终端结果不完整", err)
	}
	return terminal
}

func TestTerminalToolApprovesWritesAndPersistsCursor(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"terminal_session"}
	terminal := openToolTerminal(t, e, bot, run)
	listed := execute(t, e, bot, run, "terminal_session", map[string]any{"action": "list"})
	if listed.IsError || !strings.Contains(listed.Content, terminal.ID) {
		t.Fatal("模型无法发现已分配的终端")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan domain.ToolResultBlock, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 1}, domain.ToolUseBlock{ID: ids.New(), Name: "terminal_session", Input: map[string]any{"action": "write", "id": terminal.ID, "data": "printf 'tool%s' 'completed'; exit 0\r"}})
		results <- result
		failures <- err
	}()
	approval := pending(t, e)
	if approval.RiskLevel != "high" {
		t.Fatal("宽松模式没有提升终端输入风险")
	}
	if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.IsError {
		t.Fatal(result.Content)
	}
	for {
		wake := e.events.Wake()
		current, err := e.terminals.Get(ctx, terminal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "exited" {
			break
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("终端未退出")
		}
	}
	result := execute(t, e, bot, run, "terminal_session", map[string]any{"action": "read", "id": terminal.ID})
	var output term.SessionOutput
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || result.IsError || !strings.Contains(output.Data, "toolcompleted") {
		t.Fatal("工具未读取实际输出", err)
	}
	result = execute(t, e, bot, run, "terminal_session", map[string]any{"action": "read", "id": terminal.ID})
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || output.Data != "" {
		t.Fatal("工具游标未保存", err)
	}
	result = execute(t, e, bot, run, "terminal_session", map[string]any{"action": "close", "id": terminal.ID})
	if result.IsError {
		t.Fatal(result.Content)
	}
}

func TestTerminalToolTakeoverInvalidatesPendingApproval(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"terminal_session"}
	terminal := openToolTerminal(t, e, bot, run)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan domain.ToolResultBlock, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 1}, domain.ToolUseBlock{ID: ids.New(), Name: "terminal_session", Input: map[string]any{"action": "write", "id": terminal.ID, "data": "touch out/unapproved\r"}})
		results <- result
		failures <- err
	}()
	approval := pending(t, e)
	if _, err := e.terminals.SetController(ctx, terminal.ID, "user", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.terminals.SetController(ctx, terminal.ID, "bot", "user"); err != nil {
		t.Fatal(err)
	}
	if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	result := <-results
	if !result.IsError || !strings.Contains(result.Content, "TOOL_TARGET_CHANGED") {
		t.Fatal("旧审批仍然执行")
	}
	if _, err := e.files.Read(ctx, bot.ID, "out/unapproved", 1, 1); err == nil {
		t.Fatal("旧审批产生副作用")
	}
	if err := e.terminals.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalToolRejectsInvalidAndPrivilegedActions(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"terminal_session"}
	for _, input := range []map[string]any{
		{"action": "unknown"}, {"action": "open", "id": "unexpected"}, {"action": "open", "cols": 0}, {"action": "read"}, {"action": "read", "id": "missing", "limit": 0}, {"action": "open", "cwd": "../outside"}, {"action": "resize", "id": "missing", "cols": 80},
	} {
		if result := execute(t, e, bot, run, "terminal_session", input); !result.IsError {
			t.Fatal("无效工具参数被接受")
		}
	}
	terminal := openToolTerminal(t, e, bot, run)
	result := execute(t, e, bot, run, "terminal_session", map[string]any{"action": "write", "id": terminal.ID, "data": "sudo -n true\r"})
	if !result.IsError || !strings.Contains(result.Content, "TOOL_DENIED") {
		t.Fatal("特权终端输入没有默认拒绝")
	}
	if err := e.terminals.Close(context.Background(), terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
}
