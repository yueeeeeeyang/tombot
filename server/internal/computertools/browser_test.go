package computertools

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
)

func TestBrowserOpensOnlyAuthorizedDesktop(t *testing.T) {
	address := shellAddress()
	grant := botagent.ToolGrant{OperationID: address.OperationID, Sequence: 1, Session: address.Session, ApprovalMode: "standard", TimeoutMS: 30000, OutputBytes: 2048}
	call := botagent.ToolCall{Name: "computer_browser", Arguments: `{"action":"open"}`}
	opened := 0
	tool := Browser{open: func(ctx context.Context, session computer.ExecutionSession) error {
		if session != grant.Session {
			t.Fatal("启动请求传到其他桌面")
		}
		opened++
		return ctx.Err()
	}}
	result, err := tool.Execute(t.Context(), grant, call)
	if err != nil || result.IsError || opened != 1 {
		t.Fatal(result, err, opened)
	}
	for _, args := range []string{`{}`, `{"action":"open","display":":2"}`, `{"action":"open","action":"open"}`, `{"action":"navigate"}`, `{"action":"open","url":"https://example.test"}`} {
		invalid := call
		invalid.Arguments = args
		if r, e := tool.Execute(t.Context(), grant, invalid); e != nil || !r.IsError || opened != 1 {
			t.Fatal(r, e)
		}
	}
	restricted := grant
	restricted.ApprovalMode = "invalid"
	if r, e := tool.Execute(t.Context(), restricted, call); e == nil || opened != 1 {
		t.Fatal(r, e)
	}
	tool.open = func(context.Context, computer.ExecutionSession) error {
		return exec.Command("/bin/sh", "-c", "exit 1").Run()
	}
	if r, e := tool.Execute(t.Context(), grant, call); e != nil || !r.IsError {
		t.Fatal("未确认窗口却返回成功", r, e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := tool.Execute(ctx, grant, call); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
