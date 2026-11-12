package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/scheduler"
	"io.yueyang/tom/internal/store"
)

const routineRunColumns = `rr.id,rr.routine_id,COALESCE(rr.turn_id,''),COALESCE(rr.conversation_id,''),rr.trigger_kind,rr.status,COALESCE(t.status,''),COALESCE(rr.error_code,''),rr.scheduled_at,rr.created_at,rr.definition_json`
const routineRunJoin = ` FROM routine_runs rr LEFT JOIN turns t ON t.id=rr.turn_id `

type routineSnapshot struct {
	Definition domain.RoutineDefinition `json:"definition"`
	Revision   int64                    `json:"revision"`
}

func scanRoutineRun(row scanner) (run domain.RoutineRun, err error) {
	var scheduled, created int64
	var encoded sql.NullString
	err = row.Scan(&run.ID, &run.RoutineID, &run.TurnID, &run.ConversationID, &run.TriggerKind, &run.Status, &run.TurnStatus, &run.ErrorCode, &scheduled, &created, &encoded)
	if err != nil {
		return run, notFound(err)
	}
	run.ScheduledAt = time.UnixMilli(scheduled).UTC()
	run.CreatedAt = time.UnixMilli(created).UTC()
	if encoded.Valid {
		var snapshot routineSnapshot
		if err = json.Unmarshal([]byte(encoded.String), &snapshot); err != nil {
			return run, err
		}
		run.Definition = snapshot.Definition
		run.Revision = snapshot.Revision
	}
	return run, nil
}

// ReadRoutineRun 在调用方事务中读取不可变触发快照，供原子入队与重复触发检查共用。
func ReadRoutineRun(ctx context.Context, tx *sql.Tx, id string) (domain.RoutineRun, error) {
	return scanRoutineRun(tx.QueryRowContext(ctx, "SELECT "+routineRunColumns+routineRunJoin+"WHERE rr.id=?", id))
}

func (s *Routines) run(ctx context.Context, id string) (domain.RoutineRun, error) {
	return scanRoutineRun(s.db.QueryRowContext(ctx, "SELECT "+routineRunColumns+routineRunJoin+"WHERE rr.id=?", id))
}

// Runs 分页返回触发与实际回合状态，触发成功不等价于模型执行成功。
func (s *Routines) Runs(ctx context.Context, id, cursor string, limit int) ([]domain.RoutineRun, error) {
	if limit < 1 || limit > 200 {
		return nil, badInput("Limit must be between 1 and 200.")
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+routineRunColumns+routineRunJoin+`WHERE rr.routine_id=? AND (?='' OR rr.id<?) ORDER BY rr.id DESC LIMIT ?`, id, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []domain.RoutineRun{}
	for rows.Next() {
		run, err := scanRoutineRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// RoutineTargetBusy 检查目标全部会话；Bot 参与尚未创建成员 Run 的群回合时也视为活动。
func RoutineTargetBusy(ctx context.Context, tx *sql.Tx, target domain.RoutineTarget) (bool, error) {
	var busy bool
	if target.GroupID != "" {
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t JOIN conversations c ON c.id=t.conversation_id WHERE c.group_id=? AND t.status IN('queued','running','waiting_approval'))`, target.GroupID).Scan(&busy)
		return busy, err
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns t JOIN conversations c ON c.id=t.conversation_id WHERE t.status IN('queued','running','waiting_approval') AND (c.bot_id=? OR c.initiator_bot_id=? OR EXISTS(SELECT 1 FROM runs r WHERE r.turn_id=t.id AND r.bot_id=?) OR EXISTS(SELECT 1 FROM group_members gm WHERE gm.group_id=c.group_id AND gm.bot_id=?)))`, target.BotID, target.BotID, target.BotID, target.BotID).Scan(&busy)
	return busy, err
}

// claim 将下次时间条件推进和本次触发意图一起保存；不存在“时间已推进但无触发记录”的窗口。
func (s *Routines) claim(ctx context.Context, id, kind string, revision int64, expected time.Time, now time.Time, actor string) (string, error) {
	claimID := ""
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		r, err := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", id))
		if err != nil {
			return err
		}
		scheduled := now
		if kind == "manual" {
			if r.Revision != revision {
				return routineChanged()
			}
		} else {
			if !r.IsEnabled || r.NextRunAt == nil || !r.NextRunAt.Equal(expected) || r.NextRunAt.After(now) {
				return nil
			}
			scheduled = *r.NextRunAt
			next, err := scheduler.Next(r.Cron, r.Timezone, now)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE routines SET next_run_at=? WHERE id=? AND next_run_at=? AND is_enabled=1`, next.UnixMilli(), id, expected.UnixMilli())
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil || changed == 0 {
				return err
			}
		}
		status, code := "pending", ""
		if kind == "catch_up" && r.CatchUp == "skip" {
			status = "skipped_missed"
		}
		if status == "pending" {
			if err := validateRoutineTarget(ctx, tx, &r.RoutineDefinition); err != nil {
				status, code = "failed", routinePublicCode(err)
			}
		}
		if status == "pending" && r.OverlapPolicy == "skip" {
			busy, err := RoutineTargetBusy(ctx, tx, r.Target)
			if err != nil {
				return err
			}
			if busy {
				status = "skipped_overlap"
			}
		}
		if status == "pending" {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_runs WHERE status='pending'`).Scan(&count); err != nil {
				return err
			}
			if count >= 1024 {
				status, code = "failed", "RUN_QUEUE_FULL"
			}
		}
		conversationID := ""
		if status == "pending" {
			conversationID = r.PinnedConversationID
			if r.TargetMode == "new" {
				location, err := time.LoadLocation(r.Timezone)
				if err != nil {
					return err
				}
				conversationID = ids.New()
				conversationKind := "direct"
				if r.Target.GroupID != "" {
					conversationKind = "group"
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,source,bot_id,group_id,title,created_at,updated_at) VALUES(?,?,'routine',NULLIF(?,''),NULLIF(?,''),?,?,?)`, conversationID, conversationKind, r.Target.BotID, r.Target.GroupID, r.Name+" · "+now.In(location).Format("2006-01-02 15:04"), now.UnixMilli(), now.UnixMilli()); err != nil {
					return err
				}
				if err = AppendConversationEvent(ctx, tx, conversationID); err != nil {
					return err
				}
			}
		}
		encoded, err := json.Marshal(routineSnapshot{r.RoutineDefinition, r.Revision})
		if err != nil {
			return err
		}
		claimID = ids.New()
		if _, err = tx.ExecContext(ctx, `INSERT INTO routine_runs(id,routine_id,trigger_kind,status,scheduled_at,created_at,conversation_id,definition_json,error_code,updated_at) VALUES(?,?,?,?,?,?,NULLIF(?,''),?,NULLIF(?,''),?)`, claimID, id, kind, status, scheduled.UnixMilli(), now.UnixMilli(), conversationID, string(encoded), code, now.UnixMilli()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE routines SET last_run_at=? WHERE id=?`, now.UnixMilli(), id); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, actor, "routine.trigger", claimID, map[string]string{"trigger_kind": kind, "status": status}); err != nil {
			return err
		}
		if status == "failed" {
			if _, err = bus.Append(ctx, tx, "system.notice", conversationID, map[string]string{"routine_id": id, "routine_run_id": claimID, "level": "warning", "text": "例程触发失败，请查看运行记录。", "error_code": code}); err != nil {
				return err
			}
		}
		_, err = bus.Append(ctx, tx, "routine.updated", conversationID, map[string]string{"routine_id": id, "routine_run_id": claimID, "status": status})
		return err
	})
	if err == nil && claimID != "" {
		s.events.Notify()
	}
	return claimID, err
}

// RunNow 手动触发也校验版本、保留独立记录且不修改定时计划；队列繁忙时由后续 tick 继续尝试入队。
func (s *Routines) RunNow(ctx context.Context, id string, revision int64, actor string) (domain.RoutineRun, error) {
	claimID, err := s.claim(ctx, id, "manual", revision, time.Time{}, s.now().UTC(), actor)
	if err != nil {
		return domain.RoutineRun{}, err
	}
	if err = s.dispatch(ctx, claimID); err != nil {
		return domain.RoutineRun{}, err
	}
	return s.run(ctx, claimID)
}

func (s *Routines) dispatch(ctx context.Context, id string) error {
	run, err := s.run(ctx, id)
	if err != nil {
		return err
	}
	if run.Status != "pending" {
		return nil
	}
	err = s.dispatcher.SubmitRoutine(ctx, id)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	code := routinePublicCode(err)
	if run.Definition.OverlapPolicy == "queue" && (code == "TURN_IN_PROGRESS" || code == "RUN_QUEUE_FULL") {
		return nil
	}
	status := "failed"
	if code == "ROUTINE_OVERLAP" || code == "TURN_IN_PROGRESS" {
		status = "skipped_overlap"
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE routine_runs SET status=?,error_code=?,updated_at=? WHERE id=? AND status='pending'`, status, code, s.now().UnixMilli(), id)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		if err = store.Audit(ctx, tx, "system", "routine.dispatch_failed", id, map[string]string{"routine_id": run.RoutineID, "status": status, "error_code": code}); err != nil {
			return err
		}
		if status == "failed" {
			if _, err = bus.Append(ctx, tx, "system.notice", run.ConversationID, map[string]string{"routine_id": run.RoutineID, "routine_run_id": id, "level": "warning", "text": "例程未能入队，请查看运行记录。", "error_code": code}); err != nil {
				return err
			}
		}
		_, err = bus.Append(ctx, tx, "routine.updated", run.ConversationID, map[string]string{"routine_id": run.RoutineID, "routine_run_id": id, "status": status, "error_code": code})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

// CheckRoutineDispatch 在创建回合的同一事务中重新校验版本、目标和重叠，阻止审批后或排队期间的过期任务。
func CheckRoutineDispatch(ctx context.Context, tx *sql.Tx, id, conversationID string) error {
	run, err := ReadRoutineRun(ctx, tx, id)
	if err != nil {
		return err
	}
	if run.Status != "pending" {
		return domain.NewError(409, "ROUTINE_ALREADY_DISPATCHED", "This routine trigger is no longer pending.")
	}
	current, err := scanRoutine(tx.QueryRowContext(ctx, "SELECT "+routineColumns+" FROM routines WHERE id=?", run.RoutineID))
	if err != nil {
		return err
	}
	if current.Revision != run.Revision || (!current.IsEnabled && run.TriggerKind != "manual") {
		return routineChanged()
	}
	if run.ConversationID != conversationID {
		return domain.NewError(409, "ROUTINE_TARGET_INVALID", "The routine conversation changed.")
	}
	if err = validateRoutineTarget(ctx, tx, &run.Definition); err != nil {
		return err
	}
	if run.Definition.OverlapPolicy == "skip" {
		busy, err := RoutineTargetBusy(ctx, tx, run.Definition.Target)
		if err != nil {
			return err
		}
		if busy {
			return domain.NewError(409, "ROUTINE_OVERLAP", "The routine target already has an active turn.")
		}
	}
	if run.Definition.OverlapPolicy == "queue" {
		var earlier bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM routine_runs WHERE routine_id=? AND status='pending' AND (created_at<? OR (created_at=? AND id<?)))`, run.RoutineID, run.CreatedAt.UnixMilli(), run.CreatedAt.UnixMilli(), run.ID).Scan(&earlier); err != nil {
			return err
		}
		if earlier {
			return domain.NewError(429, "RUN_QUEUE_FULL", "An earlier trigger of this routine must enter the queue first.")
		}
	}
	return nil
}

// AttachRoutineTurn 原子关联触发与回合；重复派发不能产生第二个回合或修改已完成记录。
func AttachRoutineTurn(ctx context.Context, tx *sql.Tx, id, turnID string) error {
	if err := store.RequireChanged(tx.ExecContext(ctx, `UPDATE routine_runs SET status='triggered',turn_id=?,updated_at=? WHERE id=? AND status='pending'`, turnID, time.Now().UnixMilli(), id)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET source='routine' WHERE id=?`, turnID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET source='routine' WHERE turn_id=?`, turnID); err != nil {
		return err
	}
	_, err := bus.Append(ctx, tx, "routine.updated", "", map[string]string{"routine_run_id": id, "turn_id": turnID, "status": "triggered"})
	return err
}
