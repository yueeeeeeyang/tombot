package computertools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
)

type localBrowserStub struct {
	state       sharedbrowser.State
	pixels      []byte
	actions     int
	fail        error
	uploadError error
	command     sharedbrowser.Command
}

func (stub *localBrowserStub) State(context.Context) (sharedbrowser.State, error) {
	return stub.state, stub.fail
}
func (stub *localBrowserStub) Open(_ context.Context, id string) (sharedbrowser.Window, error) {
	stub.actions++
	window := stub.state.Windows[0]
	window.ID = id
	return window, stub.fail
}
func (stub *localBrowserStub) Execute(_ context.Context, _ string, command sharedbrowser.Command) (sharedbrowser.Window, error) {
	stub.actions++
	stub.command = command
	window := stub.state.Windows[0]
	window.Sequence = command.Sequence
	window.CommandState = "completed"
	return window, stub.fail
}
func (stub *localBrowserStub) Screenshot(context.Context, string) ([]byte, error) {
	stub.actions++
	return stub.pixels, stub.fail
}
func (stub *localBrowserStub) RespondDialog(context.Context, string, sharedbrowser.DialogReply) error {
	stub.actions++
	return stub.fail
}
func (stub *localBrowserStub) CloseWindow(context.Context, string) error {
	stub.actions++
	return stub.fail
}

func (stub *localBrowserStub) Upload(context.Context, string, sharedbrowser.UploadRequest) (sharedbrowser.Window, error) {
	stub.actions++
	if stub.uploadError != nil {
		return sharedbrowser.Window{}, stub.uploadError
	}
	return stub.state.Windows[0], stub.fail
}

func TestBrowserLocalActionsAndCompletion(t *testing.T) {
	session := shellAddress().Session
	grant := botagent.ToolGrant{OperationID: ids.New(), Sequence: 8, Session: session, ApprovalMode: "standard", TimeoutMS: 1000, OutputBytes: 4096}
	window := sharedbrowser.Window{ID: ids.New(), BotID: session.BotID, DesktopID: session.DesktopID, State: "ready"}
	stub := &localBrowserStub{state: sharedbrowser.State{Version: 2, Width: 1280, Height: 800, Windows: []sharedbrowser.Window{window}}, pixels: testDesktopPixels(t)}
	tool := &Browser{connect: func(actual computer.ExecutionSession) (browserClient, error) {
		if actual != session {
			t.Error("连接会话改变")
		}
		return stub, nil
	}}
	for _, action := range []string{"list", "open", "inspect", "screenshot", "navigate", "click", "dialog", "close"} {
		arguments := map[string]any{"action": action}
		if action != "list" && action != "open" {
			arguments["window_id"] = window.ID
		}
		if action == "navigate" {
			arguments["url"] = "https://example.com"
		}
		if action == "click" {
			arguments["x"], arguments["y"] = 1, 2
		}
		if action == "dialog" {
			stub.state.Windows[0].Dialog = sharedbrowser.Dialog{ID: 1, Kind: "confirm"}
			arguments["dialog_id"], arguments["accept"] = 1, false
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: string(encoded)})
		if err != nil || result.IsError || result.Validate(grant.OutputBytes) != nil {
			t.Fatal(action, err, result.Content)
		}
		if action == "screenshot" && (result.Artifact == nil || result.Artifact.ID != grant.OperationID || result.Artifact.BotID != session.BotID) {
			t.Fatal("截图归属改变")
		}
		if action == "click" && stub.command.Sequence != grant.Sequence {
			t.Fatal("使用模型指定序号")
		}
	}
	for _, state := range []string{"running", "waiting_dialog", "unknown"} {
		stub.state.Windows[0].Dialog = sharedbrowser.Dialog{}
		stub.state.Windows[0].CommandState = state
		if tool.Finish(t.Context()) == nil {
			t.Fatal("未完成浏览器动作报告成功", state)
		}
	}
	stub.state.Windows[0].CommandState = "completed"
	if err := tool.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	grant.Session.SessionID = ids.New()
	before := stub.actions
	if _, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: `{"action":"open"}`}); !errors.Is(err, sharedbrowser.ErrDenied) || stub.actions != before {
		t.Fatal("更换运行后继续使用原浏览器", err)
	}
}

func TestBrowserStrictArgumentsAndOwnership(t *testing.T) {
	session := shellAddress().Session
	grant := botagent.ToolGrant{OperationID: ids.New(), Sequence: 1, Session: session, ApprovalMode: "standard", TimeoutMS: 1000, OutputBytes: 1024}
	stub := &localBrowserStub{state: sharedbrowser.State{Version: 2, Width: 960, Height: 640}}
	tool := &Browser{connect: func(computer.ExecutionSession) (browserClient, error) { return stub, nil }}
	for _, arguments := range []string{`{"action":"open","action":"close"}`, `{"Action":"open"}`, `{"action":"open","window_id":null}`, `{"action":"open","x":0}`, `{"action":"click","window_id":"bad","x":1,"y":2}`, `{"action":"list","offset":-1}`, `{"action":"list","offset":1.5}`, `{"action":"open"} {}`, `{"action":"open","debug_url":"http://example.com"}`} {
		result, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: arguments})
		if err != nil || !result.IsError || stub.actions != 0 {
			t.Fatal("错误参数触发动作", arguments, err)
		}
	}
	if _, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: `{"action":"screenshot","window_id":"` + ids.New() + `"}`}); !errors.Is(err, sharedbrowser.ErrDenied) || stub.actions != 0 {
		t.Fatal("访问非所属页面", err)
	}
	stub.fail = sharedbrowser.ErrUnknown
	if _, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: `{"action":"list"}`}); err == nil {
		t.Fatal("未知结果被当作可重试工具错误")
	}
}

func TestBrowserMetadataTruncationIsExplicit(t *testing.T) {
	window := sharedbrowser.Window{ID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), State: "ready", Title: strings.Repeat("页面标题", 1000), URL: strings.Repeat("x", 8192), Dialog: sharedbrowser.Dialog{ID: 1, Kind: "prompt", Message: strings.Repeat("界面文本", 1000)}}
	result, err := browserWindowResult(window, 1024)
	if err != nil || result.IsError || len(result.Content) > 1024 || !strings.Contains(result.Content, `"truncated_fields":["url","title","dialog.message"]`) {
		t.Fatal(result.Content, err)
	}
}

func TestBrowserListFitsMinimumBudgetWithoutLosingWindows(t *testing.T) {
	session := shellAddress().Session
	grant := botagent.ToolGrant{OperationID: ids.New(), Sequence: 1, Session: session, ApprovalMode: "standard", TimeoutMS: 1000, OutputBytes: 1024}
	stub := &localBrowserStub{state: sharedbrowser.State{Version: 2, Width: 960, Height: 640}}
	for range 32 {
		stub.state.Windows = append(stub.state.Windows, sharedbrowser.Window{ID: ids.New(), State: "ready", CommandState: "waiting_dialog", Dialog: sharedbrowser.Dialog{ID: 1}})
	}
	tool := &Browser{connect: func(computer.ExecutionSession) (browserClient, error) { return stub, nil }}
	offset, seen := 0, map[string]bool{}
	for {
		arguments, err := json.Marshal(map[string]any{"action": "list", "offset": offset})
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: string(arguments)})
		if err != nil || result.IsError || len(result.Content) > 1024 {
			t.Fatal(result.Content, err)
		}
		var page struct {
			Windows    []struct{ ID string }
			NextOffset int  `json:"next_offset"`
			HasMore    bool `json:"has_more"`
		}
		if json.Unmarshal([]byte(result.Content), &page) != nil {
			t.Fatal("分页结果无法解析")
		}
		for _, window := range page.Windows {
			if seen[window.ID] {
				t.Fatal("分页重复")
			}
			seen[window.ID] = true
		}
		if !page.HasMore {
			break
		}
		if page.NextOffset <= offset {
			t.Fatal("分页未前进")
		}
		offset = page.NextOffset
	}
	if len(seen) != 32 {
		t.Fatal("分页丢失窗口", len(seen))
	}
}
