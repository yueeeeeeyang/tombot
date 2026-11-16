package botpersona

import (
	"encoding/json"
	"io.yueyang/tom/internal/provider"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ProgressInstructions = "\n\nProgress updates\nUse report_progress before substantial work and whenever the current activity changes. Summarize the activity in one short Chinese sentence for the waiting indicator. Do not disclose reasoning or include these updates in the final answer. For a simple greeting or short direct answer, reply directly."

// ProgressSchema 由运行时提供无副作用的状态工具，不受业务工具权限影响。
func ProgressSchema() provider.ToolSchema {
	return provider.ToolSchema{Name: "report_progress", Description: "Update the user's waiting indicator with one short Chinese sentence describing your current task. Report observable work only, never reasoning, secrets, credentials, private paths, or unverified completion. Use at most 60 characters without Markdown or line breaks. Call before significant work and when the task changes; batch with other tool calls when possible.", InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":60}},"required":["summary"],"additionalProperties":false}`)}
}

// ProgressSummary 只接受一项纯文本任务概括；模型的思考内容不作为工作状态。
func ProgressSummary(input any) (string, bool) {
	arguments, isObject := input.(map[string]any)
	summary, ok := arguments["summary"].(string)
	summary = strings.TrimSpace(summary)
	return summary, isObject && ok && len(arguments) == 1 && summary != "" && utf8.RuneCountInString(summary) <= 60 && !strings.ContainsFunc(summary, unicode.IsControl)
}
