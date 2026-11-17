package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botmemory"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/scheduledtasks"
)

// Tasks 返回所属猫咪的一页任务，身份来自当前有效会话。
func (service *Service) Tasks(ctx context.Context, principal identity.Principal, bot, before string) (scheduledtasks.Page, error) {
	var page scheduledtasks.Page
	err := service.user(ctx, principal, false, func(tx *sql.Tx) error {
		var err error
		page, err = scheduledtasks.List(ctx, tx, principal.UserID, bot, before, scheduledtasks.PageSize)
		return err
	})
	return page, err
}

// SetTaskEnabled 使用页面读取的版本，防止旧窗口覆盖对话中的修改。
func (service *Service) SetTaskEnabled(ctx context.Context, principal identity.Principal, bot, id string, revision int64, enabled bool) (scheduledtasks.Task, error) {
	var task scheduledtasks.Task
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		current, err := scheduledtasks.Read(ctx, tx, principal.UserID, bot, id, false)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return scheduledtasks.ErrConflict
		}
		current.IsEnabled = enabled
		task, err = scheduledtasks.Save(ctx, tx, principal.UserID, bot, id, revision, current.Definition)
		return err
	})
	return task, err
}

// DeleteTask 仅删除本人指定猫咪的任务，返回后调度索引已同步移除。
func (service *Service) DeleteTask(ctx context.Context, principal identity.Principal, bot, id string, revision int64) error {
	return service.user(ctx, principal, true, func(tx *sql.Tx) error { return scheduledtasks.Delete(ctx, tx, principal.UserID, bot, id, revision) })
}

type taskTool struct {
	executor *Service
	schema   provider.ToolSchema
	maximum  int
}

func (tool taskTool) Schema() provider.ToolSchema { return tool.schema }
func (taskTool) UsesComputer() bool               { return false }
func (taskTool) RequiresVision() bool             { return false }

// Execute 将任务变更与工具成功回执原子提交；恢复时只重放回执，不再次安排任务。
func (tool taskTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		allowed, err := memoryPermitted(ctx, tx, lease)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrUnavailable
		}
		operation, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, invocation.OperationID))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(invocation.Call)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		if invocation.Call.Name != tool.schema.Name || operation.Kind != "tool" || !bytes.Equal(operation.RequestDigest, digest[:]) || operation.Status != "started" || operation.Attempt != lease.Attempt || operation.FencingToken != lease.FencingToken {
			return ErrConflict
		}
		var output any
		if invocation.Call.Name == "task_list" {
			var query struct {
				Before string `json:"before"`
			}
			if decodeToolInput(invocation.Call.Arguments, &query, tool.schema) != nil {
				return scheduledtasks.ErrInput
			}
			output, err = scheduledtasks.List(ctx, tx, lease.UserID, lease.BotID, query.Before, 1)
		} else {
			var change struct {
				scheduledtasks.Definition
				ID               string `json:"id"`
				ExpectedRevision int64  `json:"expected_revision"`
				SourceMessageID  string `json:"source_message_id"`
				Evidence         string `json:"evidence"`
			}
			if decodeToolInput(invocation.Call.Arguments, &change, tool.schema) != nil {
				return scheduledtasks.ErrInput
			}
			if !botmemory.ValidText(change.Evidence, 1, 256) {
				return botmemory.ErrSource
			}
			if _, err = memorySource(ctx, tx, lease, botmemory.Change{SourceMessageID: change.SourceMessageID, Evidence: change.Evidence}); err != nil {
				return err
			}
			switch invocation.Call.Name {
			case "task_create":
				output, err = scheduledtasks.Save(ctx, tx, lease.UserID, lease.BotID, "", 0, change.Definition)
			case "task_update":
				output, err = scheduledtasks.Save(ctx, tx, lease.UserID, lease.BotID, change.ID, change.ExpectedRevision, change.Definition)
			case "task_delete":
				err = scheduledtasks.Delete(ctx, tx, lease.UserID, lease.BotID, change.ID, change.ExpectedRevision)
				output = map[string]any{"id": change.ID, "is_deleted": true}
			default:
				return scheduledtasks.ErrInput
			}
		}
		if err != nil {
			return err
		}
		encoded, err = json.Marshal(output)
		if err != nil {
			return err
		}
		result = botagent.ToolResult{Content: string(encoded)}
		if err = result.Validate(tool.maximum); err != nil {
			return scheduledtasks.ErrCapacity
		}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.run_operations SET status='succeeded',result=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, operation.ID, string(encoded))
		return err
	})
	if err != nil {
		message := ""
		switch {
		case errors.Is(err, scheduledtasks.ErrInput):
			message = "INVALID_TASK: Use valid concise text, an explicit IANA timezone, and either a five-field cron with run_at=null or a future RFC3339 run_at with cron=''. Follow the full schema."
		case errors.Is(err, scheduledtasks.ErrNotFound):
			message = "TASK_NOT_FOUND: This task does not exist for this cat. List tasks again."
		case errors.Is(err, scheduledtasks.ErrConflict):
			message = "TASK_CONFLICT: List again and use the current revision. Nothing changed."
		case errors.Is(err, scheduledtasks.ErrCapacity):
			message = "TASK_CAPACITY: Task count or result budget exceeded. Use pagination or remove unneeded tasks with user authorization. Nothing changed."
		case errors.Is(err, botmemory.ErrSource):
			message = "INVALID_SOURCE: Only explicit current-turn user text already delivered to you authorizes task changes. Cite its message ID and verbatim excerpt."
		}
		if message != "" {
			return botagent.ToolResult{Content: message, IsError: true}, nil
		}
	}
	return result, err
}
