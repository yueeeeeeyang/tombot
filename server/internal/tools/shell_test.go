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
	"io.yueyang/tom/internal/testkit"
)

func TestShellToolApprovalAndPersistentResult(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	bot.ToolPolicy.AllowedTools = []string{"computer_shell"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	results := make(chan domain.ToolResultBlock, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "computer_shell", Input: map[string]any{"command": "printf approved > out/report.txt; printf done"}})
		results <- result
		completed <- err
	}()
	approval := pending(t, e)
	if approval.ActionClass != "shell.exec" {
		t.Fatal("Shell 未进入正确审批类")
	}
	if _, err := e.files.Read(ctx, bot.ID, "out/report.txt", 1, 1); err == nil {
		t.Fatal("审批前执行了命令")
	}
	if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.IsError {
		t.Fatal(result.Content)
	}
	var output term.ShellResult
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || output.ExitCode != 0 || output.StdoutTail != "done" {
		t.Fatal("Shell 结果错误", err)
	}
	file, err := e.files.Read(ctx, bot.ID, "out/report.txt", 1, 1)
	if err != nil || !strings.Contains(file.Content, "approved") {
		t.Fatal("批准后命令未执行", err)
	}
	var status string
	if err = e.db.QueryRow("SELECT status FROM tool_calls WHERE tool_name='computer_shell'").Scan(&status); err != nil || status != "succeeded" {
		t.Fatal("命令状态未持久化", err)
	}
}

func TestShellSecretInjectionAndFullOutputAreRedacted(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"computer_shell"}
	value := testkit.Random(t)
	reference, err := e.secrets.Create(context.Background(), "任务环境", value, "user")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan domain.ToolResultBlock, 1)
	completed := make(chan error, 1)
	go func() {
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "computer_shell", Input: map[string]any{"command": `printf '%s\n' "$TASK_VALUE"; i=0; while [ "$i" -lt 5000 ]; do printf 'report line\n'; i=$((i+1)); done; printf '%s\n' "$TASK_VALUE"`, "env_secrets": []map[string]string{{"name": "TASK_VALUE", "secret_id": reference.ID}}}})
		results <- result
		completed <- err
	}()
	approval := pending(t, e)
	if approval.ActionClass != "secret.use" || strings.Contains(string(approval.Detail), value) {
		t.Fatal("密钥注入未提升或审批泄露了值")
	}
	if err = e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.IsError || strings.Contains(result.Content, value) {
		t.Fatal("输出泄露或执行失败")
	}
	var output term.ShellResult
	if err = json.Unmarshal([]byte(result.Content), &output); err != nil || !output.Truncated || output.OutputPath == "" {
		t.Fatal("长输出没有保留文件", err)
	}
	file, err := e.files.Read(ctx, bot.ID, output.OutputPath, 1, 1)
	if err != nil || strings.Contains(file.Content, value) || !strings.Contains(file.Content, "***") {
		t.Fatal("完整日志没有脱敏", err)
	}
	var audits int
	if err = e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='secret.use'").Scan(&audits); err != nil || audits != 1 {
		t.Fatal("注入审计缺失", err)
	}
	if err = e.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='shell.exec'").Scan(&audits); err != nil || audits != 1 {
		t.Fatal("命令执行审计缺失", err)
	}
	if err = e.db.QueryRow("SELECT COUNT(*) FROM events WHERE kind='artifact.created'").Scan(&audits); err != nil || audits != 1 {
		t.Fatal("输出产物事件缺失", err)
	}
}

func TestShellDangerousDefaultsInputBoundariesAndExitFailure(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"computer_shell"}
	for _, input := range []map[string]any{{"command": "sudo -n true"}, {"command": "true", "cwd": "../outside"}, {"command": "true", "env_secrets": []map[string]string{{"name": "TASK", "secret_id": "reference", "value": "invalid-field"}}}} {
		result := execute(t, e, bot, run, "computer_shell", input)
		if !result.IsError {
			t.Fatal("危险或无效命令被接受")
		}
	}
	result := execute(t, e, bot, run, "computer_shell", map[string]any{"command": "printf failure >&2; exit 3"})
	var output term.ShellResult
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || !result.IsError || output.ExitCode != 3 || output.StdoutTail != "failure" {
		t.Fatal("非零退出未保留失败输出", err)
	}
}

func TestShellTimeoutKeepsPartialOutputAndBoundedJSON(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"computer_shell"}
	result := execute(t, e, bot, run, "computer_shell", map[string]any{"command": "printf partial; sleep 30", "timeout_seconds": 1})
	var output term.ShellResult
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || !result.IsError || output.ErrorCode != "TOOL_TIMEOUT" || output.StdoutTail != "partial" {
		t.Fatal("超时丢失已产生输出", err)
	}
	result = execute(t, e, bot, run, "computer_shell", map[string]any{"command": `i=0; while [ "$i" -lt 20000 ]; do printf '\t'; i=$((i+1)); done`})
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil || result.IsError || len(result.Content) > 32<<10 || !output.Truncated || output.OutputPath == "" {
		t.Fatal("转义后工具结果越界", err, len(result.Content))
	}
}
