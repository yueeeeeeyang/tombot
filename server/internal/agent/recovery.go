package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"io.yueyang/tom/internal/service"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

// resolveInterruptedTools 为已落库但未回灌的调用补一个明确失败结果，重启时不会重放动作。
func resolveInterruptedTools(ctx context.Context, tx *sql.Tx, run domain.Run) error {
	rows, err := tx.QueryContext(ctx, `SELECT json_extract(b.value,'$.tool_use.id'),json_extract(b.value,'$.tool_result.tool_call_id') FROM messages m,json_each(m.blocks_json) b WHERE m.run_id=? AND json_extract(b.value,'$.kind') IN('tool_use','tool_result') ORDER BY m.seq`, run.ID)
	if err != nil {
		return err
	}
	uses := []string{}
	results := map[string]bool{}
	for rows.Next() {
		var use, result sql.NullString
		if err = rows.Scan(&use, &result); err != nil {
			break
		}
		if use.Valid {
			uses = append(uses, use.String)
		}
		if result.Valid {
			results[result.String] = true
		}
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	if closeErr != nil {
		return closeErr
	}
	blocks := []domain.MessageBlock{}
	for _, id := range uses {
		if !results[id] {
			blocks = append(blocks, domain.MessageBlock{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: id, IsError: true, Content: "TOOL_CANCELED: The run was interrupted before the tool result was saved. Do not assume the action succeeded."}})
			results[id] = true
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	message := domain.Message{ID: ids.New(), ConversationID: run.ConversationID, TurnID: &run.TurnID, RunID: &run.ID, SenderKind: "system", Blocks: blocks, Status: "complete", CreatedAt: time.Now().UTC()}
	return service.AppendMessage(ctx, tx, &message)
}

type ToolCall struct {
	ID        string          `json:"id"`
	Step      int             `json:"step"`
	Name      string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	Output    *string         `json:"output"`
	Status    string          `json:"status"`
	ErrorCode *string         `json:"error_code"`
}

// RunDetail 分页上限由运行时步数与单步调用上限共同约束。
func (r *Runtime) RunDetail(ctx context.Context, id string) (any, error) {
	run, err := r.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := r.deps.DB.QueryContext(ctx, "SELECT id,step,tool_name,input_json,output_json,status,error_code FROM tool_calls WHERE run_id=? ORDER BY step,id LIMIT ?", id, r.limits.MaxSteps*64)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calls := []ToolCall{}
	for rows.Next() {
		var call ToolCall
		var input string
		if err = rows.Scan(&call.ID, &call.Step, &call.Name, &input, &call.Output, &call.Status, &call.ErrorCode); err != nil {
			return nil, err
		}
		call.Input = json.RawMessage(input)
		calls = append(calls, call)
	}
	return struct {
		Run       domain.Run `json:"run"`
		ToolCalls []ToolCall `json:"tool_calls"`
	}{run, calls}, rows.Err()
}
