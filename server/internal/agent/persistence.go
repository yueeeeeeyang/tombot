package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

func appendConversation(ctx context.Context, tx *sql.Tx, id string) error {
	return service.AppendConversationEvent(ctx, tx, id)
}

func (r *Runtime) start(ctx context.Context, j *job) error {
	err := store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET status='running',started_at=? WHERE id=? AND status='queued'", time.Now().UnixMilli(), j.run.ID); err != nil {
			return err
		}
		started, err := tx.ExecContext(ctx, "UPDATE turns SET status='running' WHERE id=? AND status='queued'", j.run.TurnID)
		if err != nil {
			return err
		}
		changed, err := started.RowsAffected()
		if err != nil {
			return err
		}
		if changed > 0 {
			if _, err := bus.Append(ctx, tx, "turn.started", j.run.ConversationID, map[string]string{"turn_id": j.run.TurnID, "conversation_id": j.run.ConversationID}); err != nil {
				return err
			}
		}
		if _, err := bus.Append(ctx, tx, "run.started", j.run.ConversationID, map[string]string{"run_id": j.run.ID, "turn_id": j.run.TurnID, "bot_id": j.run.BotID, "model_name": j.model.DisplayName, "plan_role": j.run.PlanRole}); err != nil {
			return err
		}
		return appendConversation(ctx, tx, j.run.ConversationID)
	})
	if err == nil {
		r.deps.Events.Notify()
		r.deps.Logger.Info("开始执行", "run_id", j.run.ID, "bot_id", j.run.BotID)
	}
	return err
}

func stopNote(code string) string {
	switch code {
	case "RUN_BUDGET_EXCEEDED":
		return "已停止：达到本次执行的费用上限。"
	case "TURN_BUDGET_EXCEEDED":
		return "已停止：达到群组回合的费用上限，已完成结果仍保留。"
	case "GROUP_MEMBER_FAILED":
		return "部分群成员未能完成分工；已完成的回复与产物仍保留。"
	case "RUN_MAX_STEPS":
		return "已停止：达到本次执行的步骤上限。"
	case "RUN_TIMEOUT":
		return "已停止：达到本次执行的时间上限。"
	case "RUN_CANCELED":
		return "本次执行已取消。"
	case "RUN_INTERRUPTED":
		return "服务重启或关闭，本次执行已中断；已保存的消息仍然可用。"
	default:
		return "本次执行未完成，请检查配置后重试。错误码：" + code
	}
}

func (r *Runtime) finish(ctx context.Context, run domain.Run, status, code string) error {
	err := store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var isGroup bool
		if err := tx.QueryRowContext(ctx, "SELECT kind='group' FROM conversations WHERE id=?", run.ConversationID).Scan(&isGroup); err != nil {
			return err
		}
		var changed int
		if err := tx.QueryRowContext(ctx, `UPDATE runs SET status=?,error_code=NULLIF(?,''),finished_at=? WHERE id=? AND status IN('queued','running','waiting_approval') RETURNING 1`, status, code, now.UnixMilli(), run.ID).Scan(&changed); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		messageStatus := "complete"
		if status == "interrupted" || status == "failed" {
			messageStatus = "interrupted"
		} else if status == "canceled" {
			messageStatus = "canceled"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET status=? WHERE run_id=? AND status='streaming'", messageStatus, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tool_calls SET status='canceled',error_code=?,duration_ms=MAX(0,?-created_at),finished_at=? WHERE run_id=? AND status IN('pending','running','awaiting_approval')", code, now.UnixMilli(), now.UnixMilli(), run.ID); err != nil {
			return err
		}
		if code != "" {
			if err := resolveInterruptedTools(ctx, tx, run); err != nil {
				return err
			}
			message := domain.Message{ID: ids.New(), ConversationID: run.ConversationID, TurnID: &run.TurnID, RunID: &run.ID, SenderKind: "system", Blocks: []domain.MessageBlock{{Kind: "note", Text: stopNote(code)}}, Status: "complete", CreatedAt: now}
			if err := service.AppendMessage(ctx, tx, &message); err != nil {
				return err
			}
		}
		if !isGroup {
			if _, err := tx.ExecContext(ctx, "UPDATE turns SET status=?,error_code=NULLIF(?,''),finished_at=? WHERE id=?", status, code, now.UnixMilli(), run.TurnID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE turns SET status=CASE WHEN EXISTS(SELECT 1 FROM runs WHERE turn_id=? AND status='waiting_approval') THEN 'waiting_approval' ELSE 'running' END WHERE id=? AND status IN('queued','running','waiting_approval')`, run.TurnID, run.TurnID); err != nil {
				return err
			}
		}
		var cost int64
		if err := tx.QueryRowContext(ctx, "SELECT cost_micro FROM turns WHERE id=?", run.TurnID).Scan(&cost); err != nil {
			return err
		}
		if _, err := bus.Append(ctx, tx, "run.status", run.ConversationID, map[string]string{"run_id": run.ID, "turn_id": run.TurnID, "bot_id": run.BotID, "status": status}); err != nil {
			return err
		}
		if !isGroup {
			if _, err := bus.Append(ctx, tx, "turn.finished", run.ConversationID, map[string]any{"turn_id": run.TurnID, "status": status, "error_code": code, "cost_micro": cost}); err != nil {
				return err
			}
		}
		return appendConversation(ctx, tx, run.ConversationID)
	})
	if err == nil {
		r.deps.Events.Notify()
	}
	return err
}

// recover 有界读取未完成任务；不重放可能产生外部副作用的步骤。
func (r *Runtime) recover(ctx context.Context) error {
	for {
		rows, err := r.deps.DB.QueryContext(ctx, "SELECT id,turn_id,conversation_id,bot_id,source FROM runs WHERE status IN('running','queued','waiting_approval') ORDER BY id LIMIT 100")
		if err != nil {
			return err
		}
		runs := []domain.Run{}
		for rows.Next() {
			var run domain.Run
			if err = rows.Scan(&run.ID, &run.TurnID, &run.ConversationID, &run.BotID, &run.Source); err != nil {
				break
			}
			runs = append(runs, run)
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
		if len(runs) == 0 {
			break
		}
		for _, run := range runs {
			if err = r.finish(ctx, run, "interrupted", "RUN_INTERRUPTED"); err != nil {
				return err
			}
		}
	}
	// 群规划前和成员之间也可能中断，此时没有活跃 Run，仍须收尾持久回合。
	for {
		var id, conversationID string
		err := r.deps.DB.QueryRowContext(ctx, `SELECT id,conversation_id FROM turns WHERE status IN('queued','running','waiting_approval') ORDER BY id LIMIT 1`).Scan(&id, &conversationID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if err = r.finishGroup(ctx, id, conversationID, "interrupted", "RUN_INTERRUPTED"); err != nil {
			return err
		}
	}
}

func timestamp(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	v := time.UnixMilli(value.Int64).UTC()
	return &v
}

func (r *Runtime) Turns(ctx context.Context, conversationID, cursor string, limit int) ([]domain.Turn, error) {
	if limit < 1 || limit > 200 {
		return nil, domain.NewError(400, "INVALID_LIMIT", "Limit must be between 1 and 200.")
	}
	rows, err := r.deps.DB.QueryContext(ctx, `SELECT id,conversation_id,seq,source,status,error_code,cost_micro,created_at,finished_at,plan_json FROM turns WHERE conversation_id=? AND (?='' OR id<?) ORDER BY id DESC LIMIT ?`, conversationID, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []domain.Turn{}
	for rows.Next() {
		var turn domain.Turn
		var created int64
		var finished sql.NullInt64
		var plan sql.NullString
		if err = rows.Scan(&turn.ID, &turn.ConversationID, &turn.Seq, &turn.Source, &turn.Status, &turn.ErrorCode, &turn.CostMicro, &created, &finished, &plan); err != nil {
			return nil, err
		}
		turn.CreatedAt = time.UnixMilli(created).UTC()
		turn.FinishedAt = timestamp(finished)
		if plan.Valid {
			turn.Plan = json.RawMessage(plan.String)
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (r *Runtime) Run(ctx context.Context, id string) (domain.Run, error) {
	var run domain.Run
	var created int64
	var started, finished sql.NullInt64
	err := r.deps.DB.QueryRowContext(ctx, `SELECT id,turn_id,conversation_id,bot_id,model_id,source,status,error_code,cost_micro,budget_micro,created_at,started_at,finished_at,COALESCE(plan_role,'solo'),parent_run_id,hop_depth FROM runs WHERE id=?`, id).Scan(&run.ID, &run.TurnID, &run.ConversationID, &run.BotID, &run.ModelID, &run.Source, &run.Status, &run.ErrorCode, &run.CostMicro, &run.BudgetMicro, &created, &started, &finished, &run.PlanRole, &run.ParentRunID, &run.HopDepth)
	run.CreatedAt = time.UnixMilli(created).UTC()
	run.StartedAt = timestamp(started)
	run.FinishedAt = timestamp(finished)
	return run, notFound(err)
}
