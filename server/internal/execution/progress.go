package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botpersona"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/provider"
)

type progressTool struct{ executor *Service }

func (progressTool) Schema() provider.ToolSchema { return botpersona.ProgressSchema() }
func (progressTool) UsesComputer() bool          { return false }
func (progressTool) RequiresVision() bool        { return false }
func (tool progressTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var input any
	if json.Unmarshal([]byte(invocation.Call.Arguments), &input) != nil {
		return botagent.ToolResult{IsError: true, Content: "INVALID_PROGRESS: Provide exactly one summary string."}, nil
	}
	summary, ok := botpersona.ProgressSummary(input)
	if !ok {
		return botagent.ToolResult{IsError: true, Content: "INVALID_PROGRESS: Provide one plain Chinese sentence of 1 to 60 characters without line breaks."}, nil
	}
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if _, err := tx.ExecContext(ctx, `UPDATE tombot.runs SET progress_summary=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, summary); err != nil {
			return err
		}
		return event(ctx, tx, lease.UserID, "run.progress", lease.RunID, map[string]string{"bot_id": lease.BotID, "conversation_id": lease.ConversationID, "text": summary})
	})
	return botagent.ToolResult{Content: "Progress displayed. Continue the task."}, err
}

// Activity 读取当前会话的一项未完成运行；空列表表示空闲，不能用其他用户的会话查询。
func (service *Service) Activity(ctx context.Context, principal identity.Principal, conversationID string) ([]Run, error) {
	if !validID(conversationID) {
		return nil, ErrInvalidInput
	}
	runs := []Run{}
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID).Scan(&id); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+runColumns+` FROM tombot.runs WHERE user_id=$1 AND conversation_id=$2 AND status IN ('queued','running','canceling') ORDER BY id DESC LIMIT 1`, principal.UserID, conversationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				return errors.Join(err, rows.Close())
			}
			runs = append(runs, run)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return runs, err
}
