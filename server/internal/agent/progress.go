package agent

import (
	"context"

	"io.yueyang/tom/internal/botpersona"
	"io.yueyang/tom/internal/domain"
)

const progressToolName = "report_progress"

// reportProgress 验证模型生成的公开摘要并写入事件流；基础设施失败中止回合，参数错误回灌模型。
func (r *Runtime) reportProgress(ctx context.Context, j *job, use domain.ToolUseBlock) (domain.ToolResultBlock, error) {
	result := domain.ToolResultBlock{ToolCallID: use.ID}
	summary, ok := botpersona.ProgressSummary(use.Input)
	if !ok {
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
