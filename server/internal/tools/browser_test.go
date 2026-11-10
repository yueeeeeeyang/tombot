package tools

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
)

func TestBrowserDownloadRequiresIndividualApprovalAndPublishesArtifact(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	result := approveBrowserTool(t, e, bot, run, map[string]any{"action": "download", "id": tab.ID, "url": "https://files.example/report"}, func(a service.ApprovalSummary) {
		if a.ActionClass != "browser.interact" || a.RiskLevel != "high" || !strings.Contains(string(a.Detail), "https://files.example") {
			t.Fatal("下载审批缺少来源或高风险")
		}
		if err := e.approvals.Decide(context.Background(), a.ID, service.ApprovalDecision{Decision: "approve", Remember: &service.RememberDecision{Scope: "bot"}}, "user"); err == nil {
			t.Fatal("下载允许永久跳过确认")
		}
	})
	var downloaded service.BrowserDownload
	if err = json.Unmarshal([]byte(result.Content), &downloaded); err != nil || result.IsError || downloaded.File.Path == "" {
		t.Fatal("下载工具没有产物", err, result.Content)
	}
	file, err := e.files.Download(context.Background(), bot.ID, downloaded.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(file.File)
	closeErr := file.File.Close()
	if readErr != nil || closeErr != nil || string(content) != "测试下载内容" {
		t.Fatal("工具产物内容不正确", readErr, closeErr)
	}
	invalid := execute(t, e, bot, run, "browser", map[string]any{"action": "download", "id": tab.ID, "url": "https://files.example/report", "ref": "e1"})
	if !invalid.IsError {
		t.Fatal("下载接受含糊的双目标")
	}
}

func approveBrowserTool(t *testing.T, e *Executor, bot domain.Bot, run domain.Run, input map[string]any, inspect func(service.ApprovalSummary)) domain.ToolResultBlock {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	results := make(chan domain.ToolResultBlock, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 1}, domain.ToolUseBlock{ID: ids.New(), Name: "browser", Input: input})
		results <- result
		failures <- err
	}()
	approval := pending(t, e)
	if inspect != nil {
		inspect(approval)
	}
	if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	return <-results
}

func TestBrowserToolNewOriginAndForcedInteractionApproval(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	result := approveBrowserTool(t, e, bot, run, map[string]any{"action": "open", "url": "https://example.com"}, func(a service.ApprovalSummary) {
		if a.ActionClass != "net.new_domain" || a.RiskLevel != "high" {
			t.Fatal("新源未使用高风险审批")
		}
	})
	var tab service.BrowserTab
	if err := json.Unmarshal([]byte(result.Content), &tab); err != nil || result.IsError || tab.Controller != "bot" {
		t.Fatal("打开结果错误", err, result.Content)
	}
	for _, action := range []string{"tabs", "snapshot", "screenshot"} {
		input := map[string]any{"action": action}
		if action != "tabs" {
			input["id"] = tab.ID
		}
		result = execute(t, e, bot, run, "browser", input)
		if result.IsError {
			t.Fatal(result.Content)
		}
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "open", "id": tab.ID, "url": "https://example.com/next"})
	if result.IsError {
		t.Fatal("同源访问失败", result.Content)
	}
	if _, err := e.approvals.SaveRule(context.Background(), service.PolicyRule{BotID: bot.ID, ActionClass: "browser.interact", Decision: "allow"}, "user"); err != nil {
		t.Fatal(err)
	}
	result = approveBrowserTool(t, e, bot, run, map[string]any{"action": "type", "id": tab.ID, "ref": "e1", "text": "测试内容"}, func(a service.ApprovalSummary) {
		if a.ActionClass != "browser.interact" || !strings.Contains(string(a.Detail), `"force_approval":true`) || !strings.Contains(string(a.Detail), "测试内容") {
			t.Fatal("输入审批缺少强制标志或可审阅内容")
		}
		if err := e.approvals.Decide(context.Background(), a.ID, service.ApprovalDecision{Decision: "approve", Remember: &service.RememberDecision{Scope: "bot"}}, "user"); err == nil {
			t.Fatal("逐次确认动作可以记忆放行")
		}
	})
	if result.IsError {
		t.Fatal(result.Content)
	}
}

func TestBrowserToolTakeoverInvalidatesPendingAction(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	result := approveBrowserTool(t, e, bot, run, map[string]any{"action": "key", "id": tab.ID, "key": "Enter"}, func(a service.ApprovalSummary) {
		var err error
		tab, err = e.browser.SetController(context.Background(), tab.ID, "user", tab.Revision, "user")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.browser.SetController(context.Background(), tab.ID, "bot", tab.Revision, "user"); err != nil {
			t.Fatal(err)
		}
	})
	if !result.IsError || !strings.Contains(result.Content, "TOOL_TARGET_CHANGED") {
		t.Fatal("接管后旧审批仍执行", result.Content)
	}
	var actions int
	if err = e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='browser.action.start'").Scan(&actions); err != nil || actions != 0 {
		t.Fatal("旧审批产生了副作用", err)
	}
}

func TestBrowserToolPermissionRevokedWhileWaiting(t *testing.T) {
	for _, scenario := range []string{"新增拒绝规则", "撤回来源许可"} {
		t.Run(scenario, func(t *testing.T) {
			e, bot, run := fixture(t, "strict")
			bot.ToolPolicy.AllowedTools = []string{"browser"}
			tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, &run, "bot")
			if err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"action": "key", "id": tab.ID, "key": "Enter"}
			wantCode := "TOOL_DENIED"
			if scenario == "撤回来源许可" {
				input = map[string]any{"action": "open", "id": tab.ID, "url": "https://example.com/next"}
				wantCode = "TOOL_TARGET_CHANGED"
			}
			result := approveBrowserTool(t, e, bot, run, input, func(a service.ApprovalSummary) {
				if scenario == "新增拒绝规则" {
					if _, err := e.approvals.SaveRule(context.Background(), service.PolicyRule{BotID: bot.ID, ActionClass: "browser.interact", Decision: "deny"}, "user"); err != nil {
						t.Fatal(err)
					}
				} else {
					if a.RiskLevel != "low" {
						t.Fatal("已有来源未使用低风险审批")
					}
					if _, err := e.db.Exec("DELETE FROM browser_origin_permissions WHERE tab_id=?", tab.ID); err != nil {
						t.Fatal(err)
					}
				}
			})
			if !result.IsError || !strings.Contains(result.Content, wantCode) {
				t.Fatal("审批沿用了过时许可", result.Content)
			}
			var actions int
			if err = e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='browser.action.start'").Scan(&actions); err != nil || actions != 0 {
				t.Fatal("失效审批产生了副作用", err)
			}
		})
	}
}

func TestBrowserToolDenialInvalidArgumentsAndCrossOwner(t *testing.T) {
	t.Parallel()
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"browser"}
	for _, input := range []map[string]any{{"action": "evaluate"}, {"action": "open", "url": "file:///tmp/report"}, {"action": "key", "id": "missing", "key": "Arbitrary"}, {"action": "tabs", "id": "missing"}, {"action": "snapshot"}, {"action": "type", "id": "missing", "ref": "e1", "text": strings.Repeat("x", 16385)}} {
		result := execute(t, e, bot, run, "browser", input)
		if !result.IsError {
			t.Fatal("非法浏览器参数被接受")
		}
	}
	if _, err := e.approvals.SaveRule(context.Background(), service.PolicyRule{BotID: bot.ID, ActionClass: "net.new_domain", Decision: "deny"}, "user"); err != nil {
		t.Fatal(err)
	}
	result := execute(t, e, bot, run, "browser", map[string]any{"action": "open", "url": "https://example.com"})
	if !result.IsError || !strings.Contains(result.Content, "TOOL_DENIED") {
		t.Fatal("域名拒绝未生效")
	}
	tab, err := e.browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	result = execute(t, e, bot, run, "browser", map[string]any{"action": "snapshot", "id": tab.ID})
	if !result.IsError || !strings.Contains(result.Content, "BROWSER_TAB_FORBIDDEN") {
		t.Fatal("模型读取了未分配标签")
	}
}
