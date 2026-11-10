package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/term"
	"io.yueyang/tom/internal/testkit"
)

func TestShellToolApprovalAndPersistentResult(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	bot.ToolPolicy.AllowedTools = []string{"computer_shell"}
	ctx := context.Background()
	result := execute(t, e, bot, run, "computer_shell", map[string]any{"command": "printf approved > out/report.txt; printf done"})
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
	ctx := context.Background()
	result := execute(t, e, bot, run, "computer_shell", map[string]any{"command": `printf '%s\n' "$TASK_VALUE"; i=0; while [ "$i" -lt 5000 ]; do printf 'report line\n'; i=$((i+1)); done; printf '%s\n' "$TASK_VALUE"`, "env_secrets": []map[string]string{{"name": "TASK_VALUE", "secret_id": reference.ID}}})
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
