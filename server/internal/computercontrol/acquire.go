package computercontrol

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// Acquire 申请本机器人的桌面；可执行桌面按票号竞争用户额度，被占用的其他桌面不阻塞准入。
// 同一运行尝试只允许一条会话，已关闭的会话不能由重试重新打开。
func (service *Service) Acquire(ctx context.Context, reference RunReference) (computer.ExecutionSession, error) {
	if !reference.valid() {
		return computer.ExecutionSession{}, ErrInvalidInput
	}
	var value record
	var result error
	open := false
	created := false
	err := service.tenant.Tenant(ctx, reference.UserID, func(tx *sql.Tx) error {
		live, err := lockJob(ctx, tx, reference)
		if err != nil {
			return err
		}
		if !live {
			return ErrLeaseLost
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.computer_execution_heads(user_id,computer_id) SELECT user_id,id FROM tombot.computers WHERE user_id=$1 ON CONFLICT(user_id) DO NOTHING`, reference.UserID); err != nil {
			return err
		}
		if err = lockHead(ctx, tx, reference.UserID); err != nil {
			return err
		}
		value, err = read(ctx, tx, reference)
		if errors.Is(err, sql.ErrNoRows) {
			var ticket int64
			if err = tx.QueryRowContext(ctx, `UPDATE tombot.computer_execution_heads SET last_ticket=last_ticket+1 WHERE user_id=$1 RETURNING last_ticket`, reference.UserID).Scan(&ticket); err != nil {
				return err
			}
			value, err = scan(tx.QueryRowContext(ctx, `INSERT INTO tombot.computer_sessions(user_id,id,computer_id,node_id,container_id,generation,job_id,run_id,bot_id,job_owner,job_fencing_token,job_attempt,ticket,status,wait_deadline_at,desktop_id,linux_uid,shared_gid,display_number,wait_reason)
				SELECT c.user_id,$2,c.id,c.node_id,c.container_id,c.generation,$3,$4,$5,$6,$7,$8,$9,'waiting',clock_timestamp()+$10::bigint*interval '1 millisecond',d.id,d.linux_uid,d.shared_gid,d.display_number,'pending'
				FROM tombot.computers c JOIN tombot.bot_desktops d ON d.user_id=c.user_id AND d.computer_id=c.id
				WHERE c.user_id=$1 AND c.status='ready' AND d.bot_id=$5 RETURNING `+columns, reference.UserID, ids.New(), reference.JobID, reference.RunID, reference.BotID, reference.OwnerID, reference.FencingToken, reference.Attempt, ticket, service.options.WaitTimeout.Milliseconds()))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnavailable
			}
			if err != nil {
				return err
			}
			created = true
		} else if err != nil {
			return err
		}
		at, err := now(ctx, tx)
		if err != nil {
			return err
		}
		switch value.State {
		case "active":
			if !value.LeaseDeadline.Time.After(at) || !value.HoldDeadline.Time.After(at) {
				result = ErrLeaseLost
				return requestDrain(ctx, tx, value, "COMPUTER_LEASE_EXPIRED")
			}
			return nil
		case "starting":
			result = WaitError{Reason: WaitStartingDesktop}
			return nil
		case "released", "draining", "quarantined":
			return ErrUnavailable
		case "waiting":
			if !value.WaitDeadline.After(at) {
				result = ErrUnavailable
				return releaseWaiting(ctx, tx, value, "COMPUTER_WAIT_TIMEOUT")
			}
		}
		var matching bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.computers WHERE user_id=$1 AND id=$2 AND status='ready' AND node_id=$3 AND generation=$4 AND container_id=$5)`, value.UserID, value.ComputerID, value.NodeID, value.Generation, value.ContainerID).Scan(&matching); err != nil {
			return err
		}
		if !matching {
			result = ErrUnavailable
			return releaseWaiting(ctx, tx, value, "COMPUTER_INSTANCE_CHANGED")
		}
		reason, err := admission(ctx, tx, value)
		if err != nil {
			return err
		}
		if reason != "" {
			if created {
				var waiting int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.computer_sessions WHERE user_id=$1 AND status='waiting'`, reference.UserID).Scan(&waiting); err != nil {
					return err
				}
				if waiting > service.options.MaxWaiting {
					return ErrCapacity
				}
			}
			result = WaitError{Reason: reason}
			return setWaiting(ctx, tx, value, reason)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.desktop_execution_heads(user_id,desktop_id) VALUES($1,$2) ON CONFLICT(user_id,desktop_id) DO NOTHING`, value.UserID, value.DesktopID); err != nil {
			return err
		}
		var fence int64
		if err = tx.QueryRowContext(ctx, `UPDATE tombot.desktop_execution_heads SET last_fencing_token=last_fencing_token+1 WHERE user_id=$1 AND desktop_id=$2 RETURNING last_fencing_token`, reference.UserID, value.DesktopID).Scan(&fence); err != nil {
			return err
		}
		value, err = scan(tx.QueryRowContext(ctx, `UPDATE tombot.computer_sessions SET status='starting',wait_reason='',fencing_token=$3,revision=1,hold_deadline_at=$6::timestamptz+$4::bigint*interval '1 millisecond',lease_expires_at=$6::timestamptz+$5::bigint*interval '1 millisecond',updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING `+columns, value.UserID, value.SessionID, fence, service.options.HoldTimeout.Milliseconds(), service.options.LeaseLifetime.Milliseconds(), at))
		if err != nil {
			return err
		}
		open = true
		return sessionEvent(ctx, tx, value, "starting", "")
	})
	if err != nil {
		return computer.ExecutionSession{}, safe(err)
	}
	if result != nil {
		return computer.ExecutionSession{}, result
	}
	if open {
		command := computer.SessionCommand{Session: value.ExecutionSession, Revision: value.Revision, Lifetime: service.options.LeaseLifetime}
		if err = command.Validate(); err != nil {
			return computer.ExecutionSession{}, ErrInvalidInput
		}
		call, stop := context.WithTimeout(ctx, service.options.NodeTimeout)
		observation, callErr := service.node.Open(call, command)
		stop()
		if err = service.confirm(ctx, value, command, observation, callErr, "starting"); err != nil {
			return computer.ExecutionSession{}, err
		}
	}
	return value.ExecutionSession, nil
}
