package execution

import (
	"context"
	"database/sql"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
)

// ControlConsole 只有当前设备能申请或交还控制；观看本身不改变任务状态。
func (service *Service) ControlConsole(ctx context.Context, principal identity.Principal, id string, takeover bool) (Console, error) {
	if !validID(id) {
		return Console{}, ErrInvalidInput
	}
	var record consoleRecord
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		if err := lockDispatch(ctx, tx, principal.UserID); err != nil {
			return err
		}
		var err error
		record, err = scanConsole(tx.QueryRowContext(ctx, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND id=$2 AND device_id=$3 AND expires_at>clock_timestamp() FOR UPDATE`, principal.UserID, id, principal.DeviceID))
		if err != nil {
			return err
		}
		if takeover {
			if record.State == "waiting" || record.State == "pausing" || record.State == "active" {
				return nil
			}
			if record.State != "viewing" {
				return ErrConflict
			}
			if err = tx.QueryRowContext(ctx, `UPDATE tombot.desktop_console_heads SET revision=revision+2 WHERE user_id=$1 AND bot_id=$2 RETURNING revision`, principal.UserID, record.BotID).Scan(&record.revision); err != nil {
				return err
			}
			record, err = scanConsole(tx.QueryRowContext(ctx, `UPDATE tombot.desktop_consoles SET state='waiting',control_revision=$4,return_to_view=false WHERE user_id=$1 AND id=$2 AND device_id=$3 RETURNING `+consoleColumns, principal.UserID, id, principal.DeviceID, record.revision))
		} else {
			record, err = scanConsole(tx.QueryRowContext(ctx, `UPDATE tombot.desktop_consoles SET return_to_view=state IN ('pausing','active','resuming'),state=CASE WHEN state='waiting' THEN 'viewing' WHEN state IN ('pausing','active') THEN 'resuming' ELSE state END WHERE user_id=$1 AND id=$2 AND device_id=$3 RETURNING `+consoleColumns, principal.UserID, id, principal.DeviceID))
		}
		return err
	})
	return record.Console, err
}

// serveIdleConsole 不调用模型；有新聊天时，只读空闲桌面让出执行槽位。
func (worker *Worker) serveIdleConsole(ctx context.Context, lease Lease, session computer.ExecutionSession) error {
	for {
		if err := worker.waitConsole(ctx, lease, session); err != nil {
			return err
		}
		var live bool
		err := worker.executor.worker(ctx, lease, false, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles c SET state='closed' WHERE c.user_id=$1 AND c.run_id=$2 AND c.state IN ('opening','viewing') AND (c.expires_at<=clock_timestamp() OR NOT EXISTS(SELECT 1 FROM tombot.devices d WHERE d.user_id=c.user_id AND d.id=c.device_id AND d.revoked_at IS NULL) OR EXISTS(SELECT 1 FROM tombot.runs r WHERE r.user_id=c.user_id AND r.bot_id=c.bot_id AND r.id<>c.run_id AND r.status='queued'))`, lease.UserID, lease.RunID); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.desktop_consoles WHERE user_id=$1 AND run_id=$2 AND state NOT IN ('closed','failed'))`, lease.UserID, lease.RunID).Scan(&live)
		})
		if err != nil || !live {
			return err
		}
		if !waitPoll(ctx, 200*time.Millisecond) {
			return context.Cause(ctx)
		}
	}
}
