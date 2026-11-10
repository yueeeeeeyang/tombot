package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
)

// Tick 使用注入时钟处理到期与待入队记录；启动只补一次，旧进程未完成的入队意图明确失败而不重放。
func (s *Routines) Tick(ctx context.Context, startup bool) error {
	now := s.now().UTC()
	if startup {
		if err := s.Recover(ctx); err != nil {
			return err
		}
	}
	if err := s.expirePending(ctx, now, false); err != nil {
		return err
	}
	for batch := 0; batch < 10; batch++ {
		rows, err := s.db.QueryContext(ctx, `SELECT id,next_run_at FROM routines WHERE is_enabled=1 AND next_run_at<=? ORDER BY next_run_at,id LIMIT 100`, now.UnixMilli())
		if err != nil {
			return err
		}
		type due struct {
			id string
			at int64
		}
		pending := []due{}
		for rows.Next() {
			var item due
			if err = rows.Scan(&item.id, &item.at); err != nil {
				break
			}
			pending = append(pending, item)
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, item := range pending {
			kind := "schedule"
			if startup && item.at < now.UnixMilli() {
				kind = "catch_up"
			}
			if _, err = s.claim(ctx, item.id, kind, 0, time.UnixMilli(item.at), now, "system"); err != nil {
				return err
			}
		}
		if len(pending) < 100 {
			break
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM routine_runs WHERE status='pending' ORDER BY created_at,id LIMIT 1024`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.dispatch(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Recover 在开放管理接口之前中断旧进程的未入队意图；成功后不再清理本进程新接收的触发。
func (s *Routines) Recover(ctx context.Context) error {
	s.startupMu.Lock()
	defer s.startupMu.Unlock()
	if s.startupRecovered {
		return nil
	}
	if err := s.expirePending(ctx, s.now().UTC(), true); err != nil {
		return err
	}
	s.startupRecovered = true
	return nil
}

func (s *Routines) expirePending(ctx context.Context, now time.Time, startup bool) error {
	changed := false
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='expired',decided_at=? WHERE run_id IS NULL AND status='pending' AND expires_at<=?`, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = count > 0
		if count > 0 {
			if _, err = bus.Append(ctx, tx, "approval.resolved", "", map[string]string{"status": "expired"}); err != nil {
				return err
			}
		}
		if startup {
			result, err = tx.ExecContext(ctx, `UPDATE routine_runs SET status='failed',error_code='ROUTINE_INTERRUPTED',updated_at=? WHERE status='pending'`, now.UnixMilli())
			if err != nil {
				return err
			}
			count, err = result.RowsAffected()
			if err != nil {
				return err
			}
			changed = changed || count > 0
			if count > 0 {
				if _, err = bus.Append(ctx, tx, "system.notice", "", map[string]string{"level": "warning", "text": "服务重启前尚未入队的例程已标记中断，未重新执行；请查看例程运行记录。"}); err != nil {
					return err
				}
				_, err = bus.Append(ctx, tx, "routine.updated", "", map[string]string{"status": "failed", "error_code": "ROUTINE_INTERRUPTED"})
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && changed {
		s.events.Notify()
	}
	return err
}

// Maintain 每轮领取前等待环境就绪。维护期间不开始新一轮，已获准的轮次继续收尾。
func (s *Routines) Maintain(ctx context.Context, logger *slog.Logger, waitReady func(context.Context) error) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	startup := true
	for {
		if err := waitReady(ctx); err != nil {
			if ctx.Err() == nil {
				logger.Error("例程就绪门禁异常", "operation", "routine.tick", "error_code", routinePublicCode(err))
			}
			return
		}
		operation, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := s.Tick(operation, startup)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Error("例程调度未完成", "operation", "routine.tick", "error_code", routinePublicCode(err))
			notice, stop := context.WithTimeout(ctx, 5*time.Second)
			_, noticeErr := s.events.Publish(notice, "system.notice", "", map[string]string{"level": "error", "text": "例程调度失败，请查看运行记录与服务状态。"})
			stop()
			if noticeErr != nil {
				logger.Error("例程调度异常通知保存失败", "operation", "routine.notice")
			}
		}
		startup = startup && err != nil
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
