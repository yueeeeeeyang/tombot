package agent

import (
	"context"
	"encoding/json"
	"strings"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/orchestrator"
)

// groupFollowups 只转交明确提及的单条发言；不转发原始任务、私有工具结果或其他收件人的历史。
func (r *Runtime) groupFollowups(ctx context.Context, task orchestrator.Task, runID string) ([]orchestrator.Delivery, error) {
	rows, err := r.deps.DB.QueryContext(ctx, `SELECT id,seq,blocks_json,mentions_json,COALESCE(sender_bot_id,'') FROM messages WHERE run_id=? AND sender_kind='bot' AND status='complete' AND json_array_length(mentions_json)>0 ORDER BY seq LIMIT ?`, runID, domain.MaxGroupRunMessages+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := []orchestrator.Delivery{}
	count := 0
	for rows.Next() {
		var id, encoded, raw, sender string
		var seq int64
		if err = rows.Scan(&id, &seq, &encoded, &raw, &sender); err != nil {
			return nil, err
		}
		count++
		if count > domain.MaxGroupRunMessages {
			return nil, domain.NewError(409, "GROUP_POST_LIMIT", "The member exceeded the group message limit.")
		}
		var blocks []domain.MessageBlock
		var mentions []string
		if err = json.Unmarshal([]byte(encoded), &blocks); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &mentions); err != nil {
			return nil, err
		}
		var text strings.Builder
		for _, block := range blocks {
			if block.Kind == "text" {
				text.WriteString(block.Text)
				text.WriteByte('\n')
			}
		}
		next := task
		next.TriggerID = id
		next.HistorySeq = seq
		next.Depth = task.Depth + 1
		next.Mentions = mentions
		next.Attachments = nil
		next.Content = "Message from group member " + sender + " (quoted content):\n" + text.String()
		for _, step := range orchestrator.DirectPlan(task.Group, next.Content, mentions).Steps {
			if step.BotID != sender {
				deliveries = append(deliveries, orchestrator.Delivery{Task: next, Step: step})
			}
		}
	}
	return deliveries, rows.Err()
}
