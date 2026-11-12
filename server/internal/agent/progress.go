package agent

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

const progressToolName = "report_progress"

// progressSchema 由运行时提供无副作用的状态工具，不受业务工具权限影响。
func progressSchema() provider.ToolSchema {
	return provider.ToolSchema{Name: progressToolName, Description: "Update the user's waiting indicator with one short Chinese sentence describing your current task. Report observable work only, never reasoning, secrets, credentials, private paths, or unverified completion. Use at most 60 characters without Markdown or line breaks. Call before significant work and when the task changes; batch with other tool calls when possible.", InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":60}},"required":["summary"],"additionalProperties":false}`)}
}

// reportProgress 验证模型生成的公开摘要并写入事件流；基础设施失败中止回合，参数错误回灌模型。
func (r *Runtime) reportProgress(ctx context.Context, j *job, use domain.ToolUseBlock) (domain.ToolResultBlock, error) {
	result := domain.ToolResultBlock{ToolCallID: use.ID}
	arguments, isObject := use.Input.(map[string]any)
	summary, ok := arguments["summary"].(string)
	summary = strings.TrimSpace(summary)
	if !isObject || !ok || len(arguments) != 1 || summary == "" || utf8.RuneCountInString(summary) > 60 || strings.ContainsAny(summary, "\r\n") || strings.ContainsFunc(summary, unicode.IsControl) {
		result.IsError = true
		result.Content = "INVALID_PROGRESS: Provide one plain Chinese sentence of 1 to 60 characters without line breaks."
		return result, nil
	}
	if _, err := r.deps.Events.Publish(ctx, "run.progress", j.run.ConversationID, map[string]string{"run_id": j.run.ID, "turn_id": j.run.TurnID, "bot_id": j.run.BotID, "text": summary}); err != nil {
		return result, err
	}
	result.Content = "Progress displayed. Continue the task."
	return result, nil
}
