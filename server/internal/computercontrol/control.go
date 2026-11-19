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
			code := "COMPUTER_NODE_UNKNOWN"
			var failure *computer.Failure
			if errors.As(callErr, &failure) {
				switch failure.Code {
				case "session_computer_stopped":
					result, code = ErrStopped, "COMPUTER_STOPPED"
				case "session_computer_recovery_failed":
					result, code = ErrUnavailable, "COMPUTER_UNAVAILABLE"
				case "session_computer_recovering":
					result, code = ErrRestarting, "COMPUTER_RESTARTING"
				}
			}
			return quarantine(write, tx, value, code)
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
	return service.release(ctx, reference, false)
}

const upgradeCleanupAttempts = 3

// RecoverUpgradeSession 在登记的旧实例排空期间，对已过期任务补充有界清理尝试。
// 与正常收尾共用节点关闭、回包核验和持久状态变更，后续任务恢复由运行恢复器处理。
func (service *Service) RecoverUpgradeSession(ctx context.Context, reference RunReference) error {
	return service.release(ctx, reference, true)
}

func (service *Service) release(ctx context.Context, reference RunReference, duringUpgrade bool) error {
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
			if duringUpgrade {
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
		if value.State == "released" {
			return nil
		}
		maximumAttempts := service.options.MaxCleanupAttempts
		if duringUpgrade {
			if live {
				return ErrUnavailable
			}
			if err = allowUpgradeRecovery(ctx, tx, value); err != nil {
				return err
			}
			maximumAttempts += upgradeCleanupAttempts
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
		if value.CleanupAttempt >= maximumAttempts {
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

// allowUpgradeRecovery 校验任务已经失去原租约，且用户电脑仍处于登记的原实例排空阶段。
func allowUpgradeRecovery(ctx context.Context, tx *sql.Tx, value record) error {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computer_image_upgrades u
		JOIN tombot.computers c ON c.user_id=u.user_id AND c.id=u.computer_id AND c.node_id=u.node_id
		JOIN tombot.jobs j ON j.user_id=u.user_id AND j.id=$6
		WHERE u.user_id=$1 AND u.computer_id=$2 AND u.node_id=$3 AND u.source_generation=$4 AND u.source_container_id=$5
		AND u.state='draining' AND c.status='stopping' AND c.generation=u.source_generation AND c.container_id=u.source_container_id
		AND j.status='leased' AND j.lease_owner=$7 AND j.fencing_token=$8 AND j.attempt=$9 AND j.lease_expires_at<=clock_timestamp())`,
		value.UserID, value.ComputerID, value.NodeID, value.Generation, value.ContainerID, value.Reference.JobID,
		value.Reference.OwnerID, value.Reference.FencingToken, value.Reference.Attempt).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrUnavailable
	}
	return nil
}
