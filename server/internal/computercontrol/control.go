package computercontrol

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
)

// Check 在提交正常完成前复验执行权，防止后台因超时收尾后仍把旧控制者的结果报告成功。
// 从未申请电脑的运行只校验任务身份；节点实际回收仍必须经过 Release。
func (service *Service) Check(ctx context.Context, reference RunReference) error {
	if !reference.valid() {
		return ErrInvalidInput
	}
	return safe(service.tenant.Tenant(ctx, reference.UserID, func(tx *sql.Tx) error {
		live, err := lockJob(ctx, tx, reference)
		if err != nil {
			return err
		}
		if !live {
			return ErrLeaseLost
		}
		value, err := read(ctx, tx, reference)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		at, err := now(ctx, tx)
		if err != nil {
			return err
		}
		if value.State != "active" || !value.LeaseDeadline.Time.After(at) || !value.HoldDeadline.Time.After(at) {
			return ErrLeaseLost
		}
		return nil
	}))
}

// Renew 只延长已激活会话，不改变最大占用时间；排队或尚未申请电脑时无节点调用。
func (service *Service) Renew(ctx context.Context, reference RunReference) error {
	if !reference.valid() {
		return ErrInvalidInput
	}
	var value record
	var command computer.SessionCommand
	var result error
	renew := false
	err := service.tenant.Tenant(ctx, reference.UserID, func(tx *sql.Tx) error {
		live, err := lockJob(ctx, tx, reference)
		if err != nil {
			return err
		}
		value, err = read(ctx, tx, reference)
		if errors.Is(err, sql.ErrNoRows) {
			if !live {
				return ErrLeaseLost
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err = lockHead(ctx, tx, reference.UserID); err != nil {
			return err
		}
		// 读会话前已持有任务锁，同一运行的所有状态变更也遵守这一顺序。
		if value.State == "released" {
			return ErrLeaseLost
		}
		if !live {
			result = ErrLeaseLost
			if value.State == "waiting" {
				return releaseWaiting(ctx, tx, value, "RUN_UNAVAILABLE")
			}
			return requestDrain(ctx, tx, value, "RUN_UNAVAILABLE")
		}
		if value.State == "waiting" || value.State == "starting" {
			return nil
		}
		if value.State != "active" {
			return ErrUnavailable
		}
		at, err := now(ctx, tx)
		if err != nil {
			return err
		}
		lifetime := min(service.options.LeaseLifetime, value.HoldDeadline.Time.Sub(at))
		if !value.LeaseDeadline.Time.After(at) || lifetime < time.Second {
			result = ErrLeaseLost
			return requestDrain(ctx, tx, value, "COMPUTER_LEASE_EXPIRED")
		}
		value, err = scan(tx.QueryRowContext(ctx, `UPDATE tombot.computer_sessions SET revision=revision+1,lease_expires_at=LEAST(hold_deadline_at,clock_timestamp()+$3::bigint*interval '1 millisecond'),updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING `+columns, value.UserID, value.SessionID, lifetime.Milliseconds()))
		if err != nil {
			return err
		}
		command = computer.SessionCommand{Session: value.ExecutionSession, Revision: value.Revision, Lifetime: lifetime}
		renew = true
		return nil
	})
	if err != nil {
		return safe(err)
	}
	if result != nil {
		return result
	}
	if !renew {
		return nil
	}
	call, stop := context.WithTimeout(ctx, service.options.NodeTimeout)
	observation, callErr := service.node.Renew(call, command)
	stop()
	return service.confirm(ctx, value, command, observation, callErr, "active")
}

// confirm 仅接受仍属于当前任务和控制版本的回包；网络成功不能绕过数据库过期、取消或换代。
func (service *Service) confirm(ctx context.Context, expected record, command computer.SessionCommand, observation computer.SessionObservation, callErr error, expectedState string) error {
	var result error
	write, stop := context.WithTimeout(context.WithoutCancel(ctx), service.options.NodeTimeout)
	defer stop()
	err := service.tenant.Tenant(write, expected.UserID, func(tx *sql.Tx) error {
		live, err := lockJob(write, tx, expected.Reference)
		if err != nil {
			return err
		}
		if err = lockHead(write, tx, expected.UserID); err != nil {
			return err
		}
		value, err := read(write, tx, expected.Reference)
		if err != nil {
			return err
		}
		if value.Revision != expected.Revision || value.State != expectedState {
			return ErrLeaseLost
		}
		at, err := now(write, tx)
		if err != nil {
			return err
		}
		if !live || !value.LeaseDeadline.Time.After(at) || !value.HoldDeadline.Time.After(at) {
			result = ErrLeaseLost
			return requestDrain(write, tx, value, "COMPUTER_LEASE_EXPIRED")
		}
		if callErr != nil || !observation.Matches(command, "active") {
			result = ErrNode
			return quarantine(write, tx, value, "COMPUTER_NODE_UNKNOWN")
		}
		if expectedState == "active" {
			return nil
		}
		if _, err = tx.ExecContext(write, `UPDATE tombot.computer_sessions SET status='active',updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID); err != nil {
			return err
		}
		return sessionEvent(write, tx, value, "active", "")
	})
	if err != nil {
		return safe(err)
	}
	return result
}

func releaseWaiting(ctx context.Context, tx *sql.Tx, value record, code string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_sessions SET status='released',wait_reason='',error_code=$3,released_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID, code); err != nil {
		return err
	}
	value.WaitReason = ""
	return sessionEvent(ctx, tx, value, "released", code)
}
func requestDrain(ctx context.Context, tx *sql.Tx, value record, code string) error {
	if value.State == "draining" || value.State == "quarantined" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_sessions SET status='draining',error_code=$3,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID, code); err != nil {
		return err
	}
	return sessionEvent(ctx, tx, value, "draining", code)
}
func quarantine(ctx context.Context, tx *sql.Tx, value record, code string) error {
	// 保留首次导致停止的原因，后续清理失联不能把强制到期改成可成功恢复的普通收尾。
	if value.ErrorCode != "" {
		code = value.ErrorCode
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tombot.computer_sessions SET status='quarantined',error_code=$3,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID, code); err != nil {
		return err
	}
	return sessionEvent(ctx, tx, value, "quarantined", code)
}

// Release 允许原持有者或恢复器收尾已失效任务，但只作用于该尝试的会话。
// 未取得电脑的排队项直接结束；已发往节点的会话必须收到相同身份、相同控制版本的 drained 证据。
func (service *Service) Release(ctx context.Context, reference RunReference) error {
	if !reference.valid() {
		return ErrInvalidInput
	}
	var value record
	closeSession := false
	err := service.tenant.Tenant(ctx, reference.UserID, func(tx *sql.Tx) error {
		live, err := lockJob(ctx, tx, reference)
		if err != nil {
			return err
		}
		value, err = read(ctx, tx, reference)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = lockHead(ctx, tx, reference.UserID); err != nil {
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
			code := ""
			if !live {
				code = "RUN_UNAVAILABLE"
			} else if !value.WaitDeadline.After(at) {
				code = "COMPUTER_WAIT_TIMEOUT"
			}
			return releaseWaiting(ctx, tx, value, code)
		}
		if value.State == "active" || value.State == "starting" {
			code := ""
			if !live {
				code = "RUN_UNAVAILABLE"
			} else if !value.LeaseDeadline.Time.After(at) || !value.HoldDeadline.Time.After(at) {
				code = "COMPUTER_LEASE_EXPIRED"
			}
			if code != "" {
				if err = requestDrain(ctx, tx, value, code); err != nil {
					return err
				}
			}
		}
		if value.CleanupAttempt >= service.options.MaxCleanupAttempts {
			return ErrCleanupBlocked
		}
		// 正在进行的 Close 由数据库期限保护，其他恢复者不得同时消费重试额度或覆盖版本。
		var ready bool
		if err = tx.QueryRowContext(ctx, `SELECT next_check_at<=clock_timestamp() FROM tombot.computer_sessions WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID).Scan(&ready); err != nil {
			return err
		}
		if value.CleanupAttempt > 0 && !ready {
			return ErrWaiting
		}
		delay := min(service.options.RetryDelay*time.Duration(1<<min(value.CleanupAttempt, 6)), time.Minute) + 2*service.options.NodeTimeout
		value, err = scan(tx.QueryRowContext(ctx, `UPDATE tombot.computer_sessions SET status='draining',revision=revision+1,cleanup_attempt=cleanup_attempt+1,last_checked_at=clock_timestamp(),next_check_at=clock_timestamp()+$3::bigint*interval '1 millisecond',updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING `+columns, value.UserID, value.SessionID, delay.Milliseconds()))
		if err != nil {
			return err
		}
		closeSession = true
		return sessionEvent(ctx, tx, value, "draining", "")
	})
	if err != nil {
		return safe(err)
	}
	if !closeSession {
		return nil
	}
	command := computer.SessionCommand{Session: value.ExecutionSession, Revision: value.Revision, Lifetime: service.options.LeaseLifetime}
	call, stop := context.WithTimeout(ctx, service.options.NodeTimeout)
	observation, callErr := service.node.Close(call, command)
	stop()
	write, finish := context.WithTimeout(context.WithoutCancel(ctx), service.options.NodeTimeout)
	defer finish()
	var result error
	err = service.tenant.Tenant(write, reference.UserID, func(tx *sql.Tx) error {
		if _, err := lockJob(write, tx, reference); err != nil {
			return err
		}
		if err := lockHead(write, tx, reference.UserID); err != nil {
			return err
		}
		current, err := read(write, tx, reference)
		if err != nil {
			return err
		}
		if current.State == "released" {
			return nil
		}
		if current.Revision != value.Revision || current.State != "draining" {
			return ErrLeaseLost
		}
		if callErr != nil || !observation.Matches(command, "drained") {
			result = ErrNode
			return quarantine(write, tx, current, "COMPUTER_CLEANUP_UNKNOWN")
		}
		if _, err = tx.ExecContext(write, `UPDATE tombot.computer_sessions SET status='released',released_at=clock_timestamp(),updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, value.UserID, value.SessionID); err != nil {
			return err
		}
		return sessionEvent(write, tx, value, "released", "")
	})
	if err != nil {
		return safe(err)
	}
	return result
}
