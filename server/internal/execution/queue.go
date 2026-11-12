package execution

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const leaseColumns = `user_id,id,run_id,bot_id,conversation_id,turn_id,lease_owner,fencing_token,attempt,lease_expires_at`

func scanLease(row scanner) (Lease, error) {
	var lease Lease
	err := row.Scan(&lease.UserID, &lease.JobID, &lease.RunID, &lease.BotID, &lease.ConversationID, &lease.TurnID, &lease.OwnerID, &lease.FencingToken, &lease.Attempt, &lease.ExpiresAt)
	return lease, err
}

// Claim 先按用户最近领取时间公平选择，再领取该用户最早可执行任务。
// 过期但未完成恢复的租约仍占用机器人，不能仅凭超时启动第二个执行者。
func (service *Service) Claim(ctx context.Context, ownerID string) (Lease, error) {
	if !validID(ownerID) {
		return Lease{}, ErrInvalidInput
	}
	var lease Lease
	err := service.dispatch.Platform(ctx, func(transaction *sql.Tx) error {
		var userID string
		err := transaction.QueryRowContext(ctx, `SELECT h.user_id FROM tombot.user_dispatch_heads h WHERE EXISTS(
			SELECT 1 FROM tombot.jobs j WHERE j.user_id=h.user_id AND j.status='queued' AND j.available_at<=clock_timestamp() AND j.attempt<$1
			AND NOT EXISTS(SELECT 1 FROM tombot.jobs busy WHERE busy.user_id=j.user_id AND busy.bot_id=j.bot_id AND busy.status='leased'))
			ORDER BY h.last_claimed_at NULLS FIRST,h.user_id LIMIT 1 FOR UPDATE OF h SKIP LOCKED`, service.options.MaxAttempts).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return err
		}
		var jobID string
		err = transaction.QueryRowContext(ctx, `SELECT j.id FROM tombot.jobs j WHERE j.user_id=$1 AND j.status='queued' AND j.available_at<=clock_timestamp() AND j.attempt<$2
			AND NOT EXISTS(SELECT 1 FROM tombot.jobs busy WHERE busy.user_id=j.user_id AND busy.bot_id=j.bot_id AND busy.status='leased')
			ORDER BY j.available_at,j.created_at,j.id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`, userID, service.options.MaxAttempts).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoWork
		}
		if err != nil {
			return err
		}
		lease, err = scanLease(transaction.QueryRowContext(ctx, `UPDATE tombot.jobs SET status='leased',lease_owner=$3,lease_expires_at=clock_timestamp()+$4::bigint*interval '1 millisecond',
			fencing_token=fencing_token+1,attempt=attempt+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING `+leaseColumns, userID, jobID, ownerID, service.options.LeaseLifetime.Milliseconds()))
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `UPDATE tombot.user_dispatch_heads SET last_claimed_at=clock_timestamp() WHERE user_id=$1`, userID)
		return err
	})
	if err != nil {
		return Lease{}, translate(err)
	}
	return lease, nil
}

func validLease(lease Lease) bool {
	for _, id := range []string{lease.UserID, lease.JobID, lease.RunID, lease.BotID, lease.ConversationID, lease.TurnID, lease.OwnerID} {
		if !validID(id) {
			return false
		}
	}
	return lease.FencingToken > 0 && lease.Attempt > 0
}

// worker 在操作与提交检查期间持有任务行锁，旧持有者无法向新尝试提交结果。
func (service *Service) worker(ctx context.Context, lease Lease, allowCanceled bool, apply func(*sql.Tx) error) error {
	if !validLease(lease) {
		return ErrLeaseLost
	}
	return translate(service.tenant.Tenant(ctx, lease.UserID, func(transaction *sql.Tx) error {
		var deadline time.Time
		var canceled bool
		err := transaction.QueryRowContext(ctx, `SELECT lease_expires_at,cancel_requested_at IS NOT NULL FROM tombot.jobs
			WHERE user_id=$1 AND id=$2 AND run_id=$3 AND bot_id=$4 AND conversation_id=$5 AND turn_id=$6 AND status='leased'
			AND lease_owner=$7 AND fencing_token=$8 AND attempt=$9 AND lease_expires_at>clock_timestamp() FOR UPDATE`, lease.UserID, lease.JobID, lease.RunID, lease.BotID, lease.ConversationID, lease.TurnID, lease.OwnerID, lease.FencingToken, lease.Attempt).Scan(&deadline, &canceled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if canceled && !allowCanceled {
			return ErrCanceled
		}
		if err = apply(transaction); err != nil {
			return err
		}
		var valid bool
		if err = transaction.QueryRowContext(ctx, `SELECT clock_timestamp()<$1`, deadline).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrLeaseLost
		}
		return nil
	}))
}

// Renew 用数据库时间续约；取消请求不会被续期掩盖，Worker 应立即停止新操作并收尾。
func (service *Service) Renew(ctx context.Context, lease Lease) (Lease, error) {
	var renewed Lease
	err := service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		var err error
		renewed, err = scanLease(transaction.QueryRowContext(ctx, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()+$3::bigint*interval '1 millisecond',updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING `+leaseColumns, lease.UserID, lease.JobID, service.options.LeaseLifetime.Milliseconds()))
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return renewed, nil
}

// Recover 有界扫描真正过期的任务，逐个重新锁定核实；不重置仍有有效租约的运行。
// 已开始的外部调用标记未知，未开始调用的任务按有限次数与退避恢复。
func (service *Service) Recover(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	var expired []Lease
	err := service.dispatch.Platform(ctx, func(transaction *sql.Tx) (result error) {
		rows, err := transaction.QueryContext(ctx, `SELECT `+leaseColumns+` FROM tombot.jobs WHERE status='leased' AND lease_expires_at<=clock_timestamp() ORDER BY lease_expires_at,user_id,id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			lease, err := scanLease(rows)
			if err != nil {
				return err
			}
			expired = append(expired, lease)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, translate(err)
	}
	count := 0
	for _, lease := range expired {
		changed := false
		err = service.tenant.Tenant(ctx, lease.UserID, func(transaction *sql.Tx) error {
			var canceled bool
			err := transaction.QueryRowContext(ctx, `SELECT cancel_requested_at IS NOT NULL FROM tombot.jobs WHERE user_id=$1 AND id=$2 AND status='leased'
				AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at<=clock_timestamp() FOR UPDATE`, lease.UserID, lease.JobID, lease.OwnerID, lease.FencingToken).Scan(&canceled)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			var uncertain bool
			if pending, err := computerPending(ctx, transaction, lease); err != nil {
				return err
			} else if pending {
				return nil
			}
			lostComputer, err := computerLost(ctx, transaction, lease)
			if err != nil {
				return err
			}
			if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND status IN ('started','unknown'))`, lease.UserID, lease.RunID).Scan(&uncertain); err != nil {
				return err
			}
			if uncertain {
				if _, err = transaction.ExecContext(ctx, `UPDATE tombot.run_operations SET status='unknown',error_code='WORKER_LOST',finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND status='started'`, lease.UserID, lease.RunID); err != nil {
					return err
				}
			}
			status, code := "", ""
			switch {
			case canceled:
				status, code = "canceled", "CANCELED"
			case uncertain:
				status, code = "unknown", "EXTERNAL_RESULT_UNKNOWN"
			case lostComputer:
				status, code = "failed", "COMPUTER_LEASE_LOST"
			case lease.Attempt >= service.options.MaxAttempts:
				status, code = "failed", "RECOVERY_EXHAUSTED"
			}
			if status != "" {
				if err = finishRun(ctx, transaction, lease, Outcome{Status: status, ErrorCode: code}); err != nil {
					return err
				}
			} else {
				// 指数退避有上限；只恢复没有悬而未决外部操作的任务。
				delay := min(service.options.RecoveryDelay*time.Duration(1<<min(lease.Attempt-1, 6)), time.Minute)
				if _, err = transaction.ExecContext(ctx, `UPDATE tombot.jobs SET status='queued',lease_owner=NULL,lease_expires_at=NULL,fencing_token=fencing_token+1,
					available_at=clock_timestamp()+$3::bigint*interval '1 millisecond',updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID, delay.Milliseconds()); err != nil {
					return err
				}
				if _, err = transaction.ExecContext(ctx, `UPDATE tombot.runs SET status='queued' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID); err != nil {
					return err
				}
				if _, err = transaction.ExecContext(ctx, `UPDATE tombot.turns SET status='queued' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID); err != nil {
					return err
				}
				if err = event(ctx, transaction, lease.UserID, "run.requeued", lease.RunID, map[string]int{"attempt": lease.Attempt}); err != nil {
					return err
				}
			}
			changed = true
			return nil
		})
		if err != nil {
			return count, translate(err)
		}
		if changed {
			count++
		}
	}
	return count, nil
}

// computerPending 在任务锁内检查，清理确认前禁止逻辑完成、换代或重排。
func computerPending(ctx context.Context, transaction *sql.Tx, lease Lease) (bool, error) {
	var pending bool
	err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_sessions WHERE user_id=$1 AND job_id=$2 AND status<>'released')`, lease.UserID, lease.JobID).Scan(&pending)
	return pending, err
}

func computerLost(ctx context.Context, transaction *sql.Tx, lease Lease) (bool, error) {
	var lost bool
	err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND status='released'
		AND error_code IN ('COMPUTER_LEASE_EXPIRED','COMPUTER_WAIT_TIMEOUT','RUN_UNAVAILABLE','COMPUTER_NODE_UNKNOWN'))`, lease.UserID, lease.RunID).Scan(&lost)
	return lost, err
}
