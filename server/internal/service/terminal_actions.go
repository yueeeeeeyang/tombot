package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/term"
)

func terminalAccess(terminal Terminal, run *domain.Run, mutation bool, revision int64) error {
	if run != nil {
		if terminal.BotID != run.BotID {
			return domain.NewError(403, "TERMINAL_FORBIDDEN", "A bot may only access terminals assigned to that bot.")
		}
		if mutation && (terminal.Controller != "bot" || revision != terminal.Revision) {
			return domain.NewError(409, "TERMINAL_CONTROL_CHANGED", "Terminal control or input changed. Ask the user to return control and review the latest output before trying again.")
		}
	} else if mutation && terminal.Controller != "user" {
		return domain.NewError(409, "TERMINAL_CONTROL_REQUIRED", "Take control of the terminal before sending input.")
	}
	if terminal.Status != "running" && !(terminal.Status == "exited" && !mutation) {
		return domain.NewError(409, "TERMINAL_NOT_RUNNING", "The terminal is no longer running. Open a new terminal.")
	}
	if terminal.ExpiresAt != nil && !terminal.ExpiresAt.After(time.Now()) {
		return domain.NewError(410, "TERMINAL_EXPIRED", "The terminal retention period has expired. Open a new terminal.")
	}
	return nil
}

// Output 为客户端提供独立游标，不推进模型游标，也不延长会话保留期限。
func (s *Terminals) Output(ctx context.Context, id string, cursor int64, limit int, wait bool) (term.SessionOutput, error) {
	terminal, err := s.Get(ctx, id)
	if err != nil {
		return term.SessionOutput{}, err
	}
	if err = terminalAccess(terminal, nil, false, 0); err != nil {
		return term.SessionOutput{}, err
	}
	entry, err := s.entry(id)
	if err != nil {
		return term.SessionOutput{}, err
	}
	if err = entry.check(); err != nil {
		return term.SessionOutput{}, err
	}
	return entry.session.Read(ctx, cursor, limit, wait)
}

// ReadBot 原子推进模型游标，并将保留期限绑定到最近使用该会话的 Run。
func (s *Terminals) ReadBot(ctx context.Context, id string, run domain.Run, limit int) (term.SessionOutput, error) {
	entry, err := s.entry(id)
	if err != nil {
		return term.SessionOutput{}, err
	}
	if err = entry.acquire(ctx); err != nil {
		return term.SessionOutput{}, err
	}
	defer entry.release()
	if err = entry.check(); err != nil {
		return term.SessionOutput{}, err
	}
	terminal, err := s.Get(ctx, id)
	if err != nil {
		return term.SessionOutput{}, err
	}
	if err = terminalAccess(terminal, &run, false, 0); err != nil {
		return term.SessionOutput{}, err
	}
	output, err := entry.session.Read(ctx, terminal.ReadCursor, limit, false)
	if err != nil {
		return output, err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := activeTerminalRun(ctx, tx, &run); err != nil {
			return err
		}
		// 用户已接管的终端不重新挂到后台 Run 的清理计时器。
		_, err := tx.ExecContext(ctx, `UPDATE terminal_sessions SET read_cursor=?,run_id=CASE WHEN controller='bot' THEN ? ELSE run_id END WHERE id=?`, output.Cursor, run.ID, id)
		return err
	})
	return output, err
}

func (s *Terminals) mutate(ctx context.Context, id string, run *domain.Run, revision int64, actor, action string, details any, apply func(*term.Session) error, columns, rows int) error {
	entry, err := s.entry(id)
	if err != nil {
		return err
	}
	if err = entry.acquire(ctx); err != nil {
		return err
	}
	defer entry.release()
	if err = entry.check(); err != nil {
		return err
	}
	terminal, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err = terminalAccess(terminal, run, true, revision); err != nil {
		return err
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		runID := terminal.RunID
		if run != nil {
			if err := activeTerminalRun(ctx, tx, run); err != nil {
				return err
			}
			runID = run.ID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE terminal_sessions SET revision=revision+1,run_id=NULLIF(?,'') WHERE id=?`, runID, id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, action, id, details)
	})
	if err != nil {
		return err
	}
	if err = apply(entry.session); err != nil {
		return err
	}
	if columns > 0 {
		persist, cancel := context.WithTimeout(context.Background(), terminalPersistenceTimeout)
		defer cancel()
		err = store.Transact(persist, s.db, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(persist, `UPDATE terminal_sessions SET columns=?,rows=? WHERE id=?`, columns, rows, id); err != nil {
				return err
			}
			_, err := bus.Append(persist, tx, "terminal.updated", "", map[string]string{"terminal_id": id})
			return err
		})
		if err == nil {
			s.events.Notify()
		}
	}
	return err
}

func (s *Terminals) Write(ctx context.Context, id, input string, run *domain.Run, revision int64, actor string) error {
	if err := term.ValidateSessionInput(input); err != nil {
		return err
	}
	return s.mutate(ctx, id, run, revision, actor, "terminal.input", map[string]int{"bytes": len(input)}, func(session *term.Session) error {
		return session.Write(ctx, input)
	}, 0, 0)
}

func (s *Terminals) Resize(ctx context.Context, id string, columns, rows int, run *domain.Run, revision int64, actor string) error {
	if err := term.ValidateSize(columns, rows); err != nil {
		return err
	}
	return s.mutate(ctx, id, run, revision, actor, "terminal.resize", map[string]int{"cols": columns, "rows": rows}, func(session *term.Session) error {
		return session.Resize(ctx, columns, rows)
	}, columns, rows)
}

// SetController 仅供已认证用户显式接管或交还；递增版本使审批期间发生过输入的旧动作失效。
func (s *Terminals) SetController(ctx context.Context, id, controller, actor string) (Terminal, error) {
	if controller != "user" && controller != "bot" {
		return Terminal{}, badInput("Controller must be user or bot.")
	}
	entry, err := s.entry(id)
	if err != nil {
		return Terminal{}, err
	}
	if err = entry.acquire(ctx); err != nil {
		return Terminal{}, err
	}
	defer entry.release()
	terminal, err := s.Get(ctx, id)
	if err != nil {
		return Terminal{}, err
	}
	if err = terminalAccess(terminal, nil, false, 0); err != nil {
		return Terminal{}, err
	}
	if terminal.Status != "running" || controller == "bot" && terminal.BotID == "" {
		return Terminal{}, badInput("Return control only for a running terminal assigned to a bot.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		// 接管后由用户决定关闭；交还后在下一次模型操作时绑定新的 Run。
		if _, err := tx.ExecContext(ctx, `UPDATE terminal_sessions SET controller=?,revision=revision+1,run_id=NULL WHERE id=?`, controller, id); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "terminal.control", id, map[string]string{"controller": controller}); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "terminal.updated", "", map[string]string{"terminal_id": id, "controller": controller})
		return err
	})
	if err != nil {
		return Terminal{}, err
	}
	s.events.Notify()
	return s.Get(ctx, id)
}

func (s *Terminals) Close(ctx context.Context, id string, run *domain.Run, revision int64, actor string) error {
	return s.close(ctx, id, run, revision, actor, false)
}

func (s *Terminals) close(ctx context.Context, id string, run *domain.Run, revision int64, actor string, expiredOnly bool) error {
	entry, entryErr := s.entry(id)
	if entryErr == nil {
		if err := entry.acquire(ctx); err != nil {
			return err
		}
		defer entry.release()
	}
	terminal, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if run != nil {
		if terminal.BotID != run.BotID {
			return domain.NewError(403, "TERMINAL_FORBIDDEN", "A bot may only close terminals assigned to that bot.")
		}
		if terminal.Controller != "bot" || revision != terminal.Revision {
			return domain.NewError(409, "TERMINAL_CONTROL_CHANGED", "Terminal control or input changed. Review the terminal before closing it.")
		}
	}
	if terminal.Status == "closed" {
		return nil
	}
	if expiredOnly && (terminal.ExpiresAt == nil || terminal.ExpiresAt.After(time.Now())) {
		return nil
	}
	if entryErr != nil && terminal.Status != "interrupted" {
		return entryErr
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if run != nil {
			if err := activeTerminalRun(ctx, tx, run); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE terminal_sessions SET status='closing',revision=revision+1 WHERE id=?`, id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "terminal.close", id, map[string]bool{"expired": expiredOnly})
	})
	if err != nil {
		return err
	}
	// 用户请求取消后仍完成已经开始的关闭，避免留下不可写又无人回收的会话。
	cleanup, cancel := context.WithTimeout(context.Background(), terminalCloseTimeout)
	defer cancel()
	if entry != nil {
		if err = s.manager.Remove(cleanup, id); err != nil {
			return err
		}
		output, outputErr := entry.session.Read(cleanup, 0, 4, false)
		if outputErr != nil {
			return outputErr
		}
		terminal.ExitCode, terminal.ErrorCode = &output.ExitCode, output.ErrorCode
	}
	if err = s.updateStatus(cleanup, id, "closed", terminal.ExitCode, terminal.ErrorCode); err != nil {
		return err
	}
	if entry != nil {
		entry.closed = true
	}
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
	s.logger.Info("终端已关闭", "terminal_id", id, "bot_id", terminal.BotID)
	return nil
}

func (s *Terminals) cleanupLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(terminalCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.root.Done():
			return
		case <-ticker.C:
			if err := s.cleanupExpired(s.root); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error("过期终端清理失败", "action", "terminal.cleanup")
			}
		}
	}
}

// cleanupExpired 有界扫描已结束 Run 的会话；关闭前重新核对，避免与接管或新 Run 使用竞争。
func (s *Terminals) cleanupExpired(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id FROM terminal_sessions t JOIN runs r ON r.id=t.run_id WHERE t.status IN('running','exited','closing') AND r.finished_at<=? ORDER BY r.finished_at LIMIT ?`, time.Now().Add(-TerminalRetention).UnixMilli(), term.DefaultSessionLimit)
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
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.close(ctx, id, nil, 0, "system", true); err != nil {
			return err
		}
	}
	return nil
}
