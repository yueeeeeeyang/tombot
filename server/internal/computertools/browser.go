package computertools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
)

// BrowserSchema 只开放本桌面的启动入口，网页观察和输入复用真实桌面工具。
func BrowserSchema() provider.ToolSchema {
	return provider.ToolSchema{Name: "computer_browser", Description: "Open or focus Chrome on your own Linux desktop. Each desktop keeps its own website logins. After opening, use computer_screenshot and computer_input for all interaction, including navigation, dialogs and file selection. Downloads go to /workspace/Downloads.", InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["open"]}},"required":["action"],"additionalProperties":false}`)}
}

type Browser struct {
	open func(context.Context, computer.ExecutionSession) error
}

func NewBrowser() *Browser                 { return &Browser{open: openDesktopBrowser} }
func (*Browser) Supports(name string) bool { return name == "computer_browser" }

// openDesktopBrowser 使用授权中的桌面身份构造调用环境，不允许模型指定路径或显示号。
func openDesktopBrowser(ctx context.Context, session computer.ExecutionSession) error {
	values, err := session.DesktopEnvironment()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-B", "/usr/lib/tombot/open_browser.py")
	command.Env = []string{"PATH=/usr/bin:/bin", "DISPLAY=" + values["DISPLAY"], "XDG_RUNTIME_DIR=" + session.RuntimeDirectory()}
	return command.Run()
}

func (tool *Browser) Execute(ctx context.Context, grant botagent.ToolGrant, call botagent.ToolCall) (botagent.ToolResult, error) {
	if grant.Validate() != nil || !tool.Supports(call.Name) {
		return botagent.ToolResult{}, botagent.ErrProtocol
	}
	var compact bytes.Buffer
	if len(call.Arguments) > 1024 || json.Compact(&compact, []byte(call.Arguments)) != nil || compact.String() != `{"action":"open"}` {
		return botagent.ToolResult{IsError: true, Content: "Use action open. Use computer_screenshot and computer_input to interact with Chrome."}, nil
	}
	approval, err := policy.NeedsApproval(grant.ApprovalMode, "medium")
	if err != nil {
		return botagent.ToolResult{}, err
	}
	if approval {
		return botagent.ToolResult{IsError: true, Content: "A policy rule denied opening Chrome."}, nil
	}
	if err = tool.open(ctx, grant.Session); err != nil {
		if ctx.Err() != nil {
			return botagent.ToolResult{}, ctx.Err()
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) && exited.ExitCode() == 1 {
			return botagent.ToolResult{IsError: true, Content: "Chrome could not confirm a visible window on this desktop. Inspect your desktop before another action."}, nil
		}
		return botagent.ToolResult{}, err
	}
	return botagent.ToolResult{Content: "Chrome has a visible window on your desktop. Take a screenshot before interacting."}, nil
}
func (*Browser) Finish(ctx context.Context) error { return ctx.Err() }
