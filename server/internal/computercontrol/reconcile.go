package computercontrol

import (
	"context"
	"database/sql"
	"errors"
)

// Reconcile 有界、轮转检查会话。节点失联只进入隔离；不会把数据库超时当作进程清理证据。
// 单用户失败不阻塞其他用户收尾；达到清理次数上限后保留隔离，等待节点生命周期恢复提供新的证据。
func (service *Service) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	var references []RunReference
	err := service.dispatch.Platform(ctx, func(tx *sql.Tx) (result error) {
		rows, err := tx.QueryContext(ctx, `SELECT user_id,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt FROM tombot.computer_sessions
			WHERE status<>'released' AND next_check_at<=clock_timestamp() AND cleanup_attempt<$2 ORDER BY last_checked_at NULLS FIRST,user_id,id LIMIT $1`, limit, service.options.MaxCleanupAttempts)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			var reference RunReference
			if err = rows.Scan(&reference.UserID, &reference.JobID, &reference.RunID, &reference.BotID, &reference.OwnerID, &reference.FencingToken, &reference.Attempt); err != nil {
				return err
			}
			references = append(references, reference)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, safe(err)
	}
	count := 0
	var failures error
	for _, reference := range references {
		closeSession := false
		err = service.tenant.Tenant(ctx, reference.UserID, func(tx *sql.Tx) error {
			live, err := lockJob(ctx, tx, reference)
			if err != nil {
				return err
			}
			if err = lockHead(ctx, tx, reference.UserID); err != nil {
				return err
			}
			value, err := read(ctx, tx, reference)
			if err != nil {
				return err
			}
			if value.State == "released" {
				return nil
			}
			at, err := now(ctx, tx)
			if err != nil {
				return err
			}
			if value.State == "waiting" {
				if !live || !value.WaitDeadline.After(at) {
					closeSession = true
				}
			} else if !live || !value.LeaseDeadline.Time.After(at) || !value.HoldDeadline.Time.After(at) || value.State == "draining" || value.State == "quarantined" {
				closeSession = true
			}
			// 有效任务轮转到队尾；待清理项的下一次检查时间由 Close 的退避控制。
			_, err = tx.ExecContext(ctx, `UPDATE tombot.computer_sessions SET last_checked_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID)
			return err
		})
		if err == nil && closeSession {
			err = service.Release(ctx, reference)
			if err == nil {
				count++
			}
		}
		if err != nil && !errors.Is(err, ErrWaiting) {
			service.logger.WarnContext(ctx, "电脑会话清理未确认，保持隔离", "user_id", reference.UserID, "run_id", reference.RunID, "job_id", reference.JobID)
			failures = errors.Join(failures, safe(err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return count, failures
}
