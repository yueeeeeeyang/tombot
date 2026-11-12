package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	safelog "io.yueyang/tom/internal/infra/log"
)

const activityColumns = "id,run_id,tool_name,step,status,duration_ms,COALESCE(error_code,''),input_json"

func scanToolActivity(row interface{ Scan(...any) error }) (domain.ToolActivity, error) {
	var activity domain.ToolActivity
	var preview string
	err := row.Scan(&activity.ID, &activity.RunID, &activity.Name, &activity.Step, &activity.Status, &activity.DurationMS, &activity.ErrorCode, &preview)
	if err != nil {
		return activity, err
	}
	if err := json.Unmarshal([]byte(preview), &activity.InputPreview); err != nil {
		return activity, errors.New("工具参数摘要格式无效")
	}
	activity.InputPreview.Path = safelog.SafeText(activity.InputPreview.Path)
	activity.InputPreview.Tool = safelog.SafeText(activity.InputPreview.Tool)
	return activity, nil
}

// AppendToolActivity 把权威工具状态和 SSE 游标一起提交；调用方在事务成功后唤醒订阅者。
func AppendToolActivity(ctx context.Context, tx *sql.Tx, conversationID, callID, kind string) error {
	activity, err := scanToolActivity(tx.QueryRowContext(ctx, "SELECT "+activityColumns+" FROM tool_calls WHERE id=?", callID))
	if err != nil {
		return err
	}
	_, err = bus.Append(ctx, tx, kind, conversationID, activity)
	return err
}

// attachToolActivities 使用已有主键和运行索引批量装配当前页及活跃调用。正文分页边界可能恰好分开调用与结果。
func attachToolActivities(ctx context.Context, tx *sql.Tx, messages []domain.Message, runs []ActiveRun) error {
	owners := make(map[string][]*domain.Message)
	var ids, runIDs []string
	for i := range messages {
		m := &messages[i]
		for _, block := range m.Blocks {
			var id string
			switch {
			case block.ToolUse != nil:
				id = block.ToolUse.ID
			case block.ToolResult != nil:
				id = block.ToolResult.ToolCallID
			default:
				continue
			}
			if len(owners[id]) == 0 {
				ids = append(ids, id)
			}
			owners[id] = append(owners[id], m)
		}
	}
	active := make(map[string]*ActiveRun, len(runs))
	for i := range runs {
		runIDs = append(runIDs, runs[i].ID)
		active[runs[i].ID] = &runs[i]
	}
	if len(ids) == 0 && len(runIDs) == 0 {
		return nil
	}
	calls, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	running, err := json.Marshal(runIDs)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+activityColumns+" FROM tool_calls WHERE id IN (SELECT value FROM json_each(?)) OR (run_id IN (SELECT value FROM json_each(?)) AND status IN ('pending','running','awaiting_approval'))", string(calls), string(running))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanToolActivity(rows)
		if err != nil {
			return err
		}
		for _, m := range owners[item.ID] {
			if m.RunID != nil && *m.RunID == item.RunID {
				m.ToolCalls = append(m.ToolCalls, item)
			}
		}
		run := active[item.RunID]
		if run != nil && (item.Status == "pending" || item.Status == "running" || item.Status == "awaiting_approval") {
			if run.CurrentTool != nil {
				return errors.New("同一执行出现多项进行中的工具调用")
			}
			run.CurrentTool = &item
		}
	}
	return rows.Err()
}
