package execution

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computercontrol"
)

type ConsoleRuntime interface {
	computer.ConsoleRuntime
	computer.DesktopRuntime
}

// waitConsole 只在模型/工具操作边界交接；已执行但尚未结算的副作用不会与用户输入并发。
func (worker *Worker) waitConsole(ctx context.Context, lease Lease, session computer.ExecutionSession) (failure error) {
	var record consoleRecord
	err := worker.executor.worker(ctx, lease, false, func(tx *sql.Tx) error {
		var err error
		record, err = scanConsole(tx.QueryRowContext(ctx, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND run_id=$2 AND state IN ('opening','waiting','pausing','active','resuming') ORDER BY created_at LIMIT 1 FOR UPDATE`, lease.UserID, lease.RunID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if record.stateActive() && record.sessionID != "" && record.sessionID != session.SessionID {
			return ErrUncertain
		}
		if record.State == "waiting" || record.State == "opening" {
			if !record.ExpiresAt.After(time.Now()) {
				_, err = tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state='closed' WHERE user_id=$1 AND id=$2`, lease.UserID, record.ID)
				record.ID = ""
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state=CASE WHEN state='opening' THEN state ELSE 'pausing' END,computer_session_id=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, record.ID, session.SessionID)
			if record.State != "opening" {
				record.State = "pausing"
			}
		}
		return err
	})
	if err != nil || record.ID == "" {
		return err
	}
	if record.State == "resuming" && record.sessionID == "" {
		return worker.closeConsoleRecord(ctx, lease, record.ID, "closed", "")
	}
	runtime, ok := worker.agents.(ConsoleRuntime)
	if !ok {
		return ErrUnavailable
	}
	defer func() {
		if failure != nil {
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), worker.options.RecordTimeout)
			defer cancel()
			failure = errors.Join(failure, worker.closeConsoleRecord(bounded, lease, record.ID, "failed", "DESKTOP_CONTROL_FAILED"))
		}
	}()
	start, stop := context.WithTimeout(ctx, 45*time.Second)
	status, err := runtime.StartDesktop(start, session)
	for err == nil && status.State == "starting" {
		if !waitPoll(start, 100*time.Millisecond) {
			err = start.Err()
			break
		}
		status, err = runtime.InspectDesktop(start, session)
	}
	stop()
	if err != nil || status.State != "ready" {
		return errors.Join(ErrUnavailable, err)
	}
	if record.State == "opening" {
		return worker.executor.worker(ctx, lease, false, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state='viewing' WHERE user_id=$1 AND id=$2 AND state='opening' AND expires_at>clock_timestamp()`, lease.UserID, record.ID)
			return err
		})
	}
	call, cancel := context.WithTimeout(ctx, worker.options.AgentTimeout)
	err = callConsoleControl(call, runtime, computer.ConsoleRequest{Session: session, ViewerID: record.ID, Revision: record.revision, Action: "prepare"})
	cancel()
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	call, cancel = context.WithTimeout(ctx, worker.options.AgentTimeout)
	err = callConsoleControl(call, runtime, computer.ConsoleRequest{Session: session, ViewerID: record.ID, Revision: record.revision, Action: "pause"})
	cancel()
	if err != nil {
		return errors.Join(ErrUncertain, err)
	}
	defer func() {
		resume, stop := context.WithTimeout(context.WithoutCancel(ctx), worker.options.AgentTimeout)
		defer stop()
		err := callConsoleControl(resume, runtime, computer.ConsoleRequest{Session: session, ViewerID: record.ID, Revision: record.revision + 1, Action: "resume"})
		if err != nil {
			failure = errors.Join(failure, ErrUncertain, err)
			return
		}
		if failure == nil {
			failure = worker.executor.worker(resume, lease, true, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(resume, `UPDATE tombot.desktop_consoles SET state=CASE WHEN return_to_view AND expires_at>clock_timestamp() THEN 'viewing' ELSE 'closed' END,return_to_view=false WHERE user_id=$1 AND id=$2 AND state<>'failed'`, lease.UserID, record.ID)
				return err
			})
		}
	}()
	err = worker.executor.worker(ctx, lease, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state='active' WHERE user_id=$1 AND id=$2 AND state='pausing' AND expires_at>clock_timestamp()`, lease.UserID, record.ID)
		return err
	})
	if err != nil {
		return err
	}
	for {
		var active bool
		err = worker.executor.worker(ctx, lease, false, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT state='active' AND expires_at>clock_timestamp() AND EXISTS(SELECT 1 FROM tombot.devices d WHERE d.user_id=c.user_id AND d.id=c.device_id AND d.revoked_at IS NULL) FROM tombot.desktop_consoles c WHERE user_id=$1 AND id=$2`, lease.UserID, record.ID).Scan(&active)
		})
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if !waitPoll(ctx, 200*time.Millisecond) {
			return context.Cause(ctx)
		}
	}
}

func (record consoleRecord) stateActive() bool {
	return record.State == "pausing" || record.State == "active" || record.State == "resuming"
}

func (worker *Worker) closeConsoleRecord(ctx context.Context, lease Lease, id, state, code string) error {
	return worker.executor.worker(ctx, lease, true, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state=CASE WHEN state='failed' THEN state ELSE $3 END,error_code=CASE WHEN state='failed' OR $4='' THEN error_code ELSE $4 END WHERE user_id=$1 AND id=$2`, lease.UserID, id, state, code)
		return err
	})
}

func (worker *Worker) acquireConsoleComputer(ctx context.Context, lease Lease) (computer.ExecutionSession, error) {
	for {
		session, err := worker.computers.Acquire(ctx, computerReference(lease))
		if err == nil {
			return session, nil
		}
		if !errors.Is(err, computercontrol.ErrWaiting) {
			return session, err
		}
		if !waitPoll(ctx, worker.options.PollInterval) {
			return session, context.Cause(ctx)
		}
	}
}

// finishConsoles 与受理端锁定同一运行行，关闭接管入口之后才能释放桌面，消除收尾时的悬挂请求。
func (worker *Worker) finishConsoles(ctx context.Context, lease Lease, canContinue bool) error {
	for {
		var waiting bool
		var session computer.ExecutionSession
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), worker.options.RecordTimeout)
		err := worker.executor.worker(bounded, lease, true, func(tx *sql.Tx) error {
			var id string
			if err := tx.QueryRowContext(bounded, `SELECT id FROM tombot.runs WHERE user_id=$1 AND id=$2 FOR UPDATE`, lease.UserID, lease.RunID).Scan(&id); err != nil {
				return err
			}
			if canContinue && ctx.Err() == nil {
				record, err := scanConsole(tx.QueryRowContext(bounded, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND run_id=$2 AND state IN ('opening','waiting','pausing','active','resuming') LIMIT 1`, lease.UserID, lease.RunID))
				if err == nil {
					if record.sessionID == "" {
						if err = tx.QueryRowContext(bounded, `SELECT id FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND status='active'`, lease.UserID, lease.RunID).Scan(&record.sessionID); err != nil {
							return err
						}
					}
					session, err = consoleSession(bounded, tx, lease.UserID, record)
					waiting = err == nil
					return err
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if _, err := tx.ExecContext(bounded, `UPDATE tombot.runs SET accepts_console=false WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID); err != nil {
				return err
			}
			_, err := tx.ExecContext(bounded, `UPDATE tombot.desktop_consoles SET state=CASE WHEN state IN ('opening','viewing') THEN 'closed' ELSE 'failed' END,error_code='RUN_ENDED' WHERE user_id=$1 AND run_id=$2 AND state IN ('opening','viewing','waiting','pausing','active','resuming')`, lease.UserID, lease.RunID)
			return err
		})
		cancel()
		if err != nil {
			return err
		}
		if !waiting {
			return nil
		}
		if err = worker.waitConsole(ctx, lease, session); err != nil {
			return err
		}
	}
}

func callConsoleControl(ctx context.Context, runtime computer.ConsoleRuntime, request computer.ConsoleRequest) error {
	receipt, err := runtime.Console(ctx, request)
	if err != nil {
		return err
	}
	return receipt.Validate(request)
}
