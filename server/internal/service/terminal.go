package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/term"
	"io.yueyang/tom/internal/workspace"
)

const TerminalRetention = 30 * time.Minute
const terminalCleanupInterval = time.Minute
const terminalPersistenceTimeout = 5 * time.Second
const terminalCloseTimeout = 15 * time.Second

type Terminal struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	BotID      string     `json:"bot_id,omitempty"`
	RunID      string     `json:"run_id,omitempty"`
	Title      string     `json:"title"`
	Cwd        string     `json:"cwd"`
	Status     string     `json:"status"`
	Controller string     `json:"controller"`
	Revision   int64      `json:"revision"`
	Columns    int        `json:"cols"`
	Rows       int        `json:"rows"`
	ReadCursor int64      `json:"read_cursor"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	ErrorCode  string     `json:"error_code,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type TerminalOpenInput struct {
	BotID   string `json:"bot_id"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title"`
	Columns int    `json:"cols"`
	Rows    int    `json:"rows"`
}

type terminalEntry struct {
	lock      chan struct{}
	session   *term.Session
	failureMu sync.Mutex
	failure   error
	closed    bool
}

func (entry *terminalEntry) check() error {
	entry.failureMu.Lock()
	defer entry.failureMu.Unlock()
	if entry.failure != nil {
		return domain.NewError(503, "TERMINAL_STATE_FAILED", "The terminal exit state could not be saved. Close this terminal before continuing.")
	}
	return nil
}

func (entry *terminalEntry) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case entry.lock <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		entry.release()
		return err
	}
	return nil
}
func (entry *terminalEntry) release() { <-entry.lock }

// Terminals 保存会话元数据、接管权限和模型读取游标；每个会话的输入与权限变更共用同一把锁。
type Terminals struct {
	db      *sql.DB
	events  *bus.Bus
	bots    *Bots
	files   *workspace.Files
	logger  *slog.Logger
	manager *term.Manager
	root    context.Context
	stop    context.CancelFunc
	mu      sync.Mutex
	entries map[string]*terminalEntry
	wg      sync.WaitGroup
}

func NewTerminals(ctx context.Context, db *sql.DB, events *bus.Bus, bots *Bots, files *workspace.Files, logger *slog.Logger) (*Terminals, error) {
	if db == nil || events == nil || bots == nil || files == nil || logger == nil {
		return nil, errors.New("终端服务依赖不完整")
	}
	root, stop := context.WithCancel(ctx)
	s := &Terminals{db: db, events: events, bots: bots, files: files, logger: logger, root: root, stop: stop, manager: term.NewManager(root), entries: map[string]*terminalEntry{}}
	if err := s.recover(ctx); err != nil {
		stop()
		return nil, err
	}
	s.wg.Add(1)
	go s.cleanupLoop()
	return s, nil
}

func (s *Terminals) recover(ctx context.Context) error {
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE terminal_sessions SET status='interrupted',error_code='TERMINAL_INTERRUPTED',finished_at=COALESCE(finished_at,?),revision=revision+1 WHERE status IN('starting','running','closing','exited')`, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if err = store.Audit(ctx, tx, "system", "terminal.recover", "", map[string]int64{"count": count}); err != nil {
			return err
		}
		_, err = bus.Append(ctx, tx, "terminal.updated", "", map[string]bool{"resync": true})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

const terminalSelect = `SELECT t.sequence,t.id,t.owner,COALESCE(t.bot_id,''),COALESCE(t.run_id,''),t.title,t.cwd,t.status,t.controller,t.revision,t.columns,t.rows,t.read_cursor,t.exit_code,COALESCE(t.error_code,''),t.created_at,t.finished_at,r.finished_at FROM terminal_sessions t LEFT JOIN runs r ON r.id=t.run_id `

func scanTerminal(row interface{ Scan(...any) error }) (Terminal, error) {
	var terminal Terminal
	var sequence, created int64
	var finished, runFinished sql.NullInt64
	err := row.Scan(&sequence, &terminal.ID, &terminal.Owner, &terminal.BotID, &terminal.RunID, &terminal.Title, &terminal.Cwd, &terminal.Status, &terminal.Controller, &terminal.Revision, &terminal.Columns, &terminal.Rows, &terminal.ReadCursor, &terminal.ExitCode, &terminal.ErrorCode, &created, &finished, &runFinished)
	if err != nil {
		return Terminal{}, notFound(err)
	}
	prefix := "tom-user"
	if strings.HasPrefix(terminal.Owner, "bot:") {
		prefix = "tom-bot-" + terminal.BotID[:min(8, len(terminal.BotID))]
	}
	terminal.Name = fmt.Sprintf("%s-%d", prefix, sequence)
	terminal.CreatedAt = time.UnixMilli(created).UTC()
	if finished.Valid {
		value := time.UnixMilli(finished.Int64).UTC()
		terminal.FinishedAt = &value
	}
	if runFinished.Valid {
		value := time.UnixMilli(runFinished.Int64).Add(TerminalRetention).UTC()
		terminal.ExpiresAt = &value
	}
	return terminal, nil
}

func (s *Terminals) Get(ctx context.Context, id string) (Terminal, error) {
	return scanTerminal(s.db.QueryRowContext(ctx, terminalSelect+`WHERE t.id=?`, id))
}

func (s *Terminals) List(ctx context.Context, botID, cursor string, limit int) ([]Terminal, error) {
	if limit < 1 || limit > 200 {
		return nil, badInput("Limit must be between 1 and 200.")
	}
	rows, err := s.db.QueryContext(ctx, terminalSelect+`WHERE (?='' OR t.bot_id=?) AND (?='' OR t.id<?) ORDER BY t.id DESC LIMIT ?`, botID, botID, cursor, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terminals := []Terminal{}
	for rows.Next() {
		terminal, err := scanTerminal(rows)
		if err != nil {
			return nil, err
		}
		terminals = append(terminals, terminal)
	}
	return terminals, rows.Err()
}

func (s *Terminals) entry(id string) (*terminalEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[id]
	if !exists {
		return nil, domain.NewError(409, "TERMINAL_NOT_RUNNING", "This terminal is unavailable. Terminal processes and output are not restored after a server restart.")
	}
	return entry, nil
}

// Open 用户可选择共享或 Bot 工作区；模型只能以其当前 Run 在自己的工作区创建会话。
func (s *Terminals) Open(ctx context.Context, input TerminalOpenInput, run *domain.Run, actor string) (Terminal, error) {
	if input.Columns == 0 {
		input.Columns = 100
	}
	if input.Rows == 0 {
		input.Rows = 30
	}
	if err := term.ValidateSize(input.Columns, input.Rows); err != nil {
		return Terminal{}, err
	}
	input.Title = strings.TrimSpace(input.Title)
	if len(input.Title) > 120 || !utf8.ValidString(input.Title) || strings.ContainsAny(input.Title, "\x00\r\n") {
		return Terminal{}, badInput("Terminal titles must contain at most 120 bytes without line breaks.")
	}
	owner, controller, runID := "user", "user", ""
	if run != nil {
		if input.BotID != "" && input.BotID != run.BotID {
			return Terminal{}, domain.NewError(403, "TERMINAL_FORBIDDEN", "A bot may only open terminals in its own workspace.")
		}
		input.BotID, owner, controller, runID = run.BotID, "bot:"+run.BotID, "bot", run.ID
	}
	if input.Title == "" {
		input.Title = "终端"
	}
	var home, cwd string
	var err error
	if input.BotID != "" {
		bot, botErr := s.bots.Get(ctx, input.BotID)
		if botErr != nil {
			return Terminal{}, botErr
		}
		if bot.IsArchived {
			return Terminal{}, badInput("Open terminals only for an active bot.")
		}
		cwd, err = s.files.Directory(input.BotID, input.Cwd)
		if err == nil {
			home, err = s.files.CommandHome(input.BotID)
		}
	} else {
		if filepath.IsAbs(input.Cwd) {
			return Terminal{}, badInput("User terminal paths must be relative to the shared workspace.")
		}
		path := input.Cwd
		if path == "shared" {
			path = ""
		} else {
			path = strings.TrimPrefix(path, "shared/")
		}
		cwd, err = s.files.Directory("user", "shared/"+path)
		if err == nil {
			home, err = s.files.SharedCommandHome()
		}
	}
	if err != nil {
		return Terminal{}, err
	}
	id, now := ids.New(), time.Now()
	// 登记与进程创建受同一管理锁保护，关闭不会漏掉处于启动阶段的会话。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root.Err() != nil {
		return Terminal{}, domain.NewError(503, "TERMINALS_STOPPING", "The terminal service is stopping.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if run != nil {
			if err := activeTerminalRun(ctx, tx, run); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO terminal_sessions(id,owner,bot_id,run_id,title,cwd,status,controller,columns,rows,created_at) VALUES(?,?,NULLIF(?,''),NULLIF(?,''),?,?,'starting',?,?,?,?)`, id, owner, input.BotID, runID, input.Title, cwd, controller, input.Columns, input.Rows, now.UnixMilli()); err != nil {
			return store.ConstraintError(err)
		}
		return store.Audit(ctx, tx, actor, "terminal.open", id, map[string]string{"bot_id": input.BotID})
	})
	if err != nil {
		return Terminal{}, err
	}
	session, startErr := s.manager.Open(ctx, term.SessionOptions{ID: id, Owner: owner, Cwd: cwd, Home: home, Columns: input.Columns, Rows: input.Rows})
	persist, cancel := context.WithTimeout(context.Background(), terminalPersistenceTimeout)
	defer cancel()
	if startErr != nil {
		_, updateErr := s.db.ExecContext(persist, `UPDATE terminal_sessions SET status='closed',error_code='TERMINAL_START_FAILED',finished_at=? WHERE id=?`, time.Now().UnixMilli(), id)
		return Terminal{}, errors.Join(startErr, updateErr)
	}
	entry := &terminalEntry{lock: make(chan struct{}, 1), session: session}
	s.entries[id] = entry
	if err = s.updateStatus(persist, id, "running", nil, ""); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), terminalCloseTimeout)
		defer closeCancel()
		closeErr := s.manager.Remove(closeCtx, id)
		if closeErr == nil {
			delete(s.entries, id)
			closeErr = s.updateStatus(closeCtx, id, "closed", nil, "TERMINAL_STATE_FAILED")
		}
		return Terminal{}, errors.Join(err, closeErr)
	}
	s.wg.Add(1)
	go s.watch(id, entry)
	s.logger.Info("终端已创建", "terminal_id", id, "bot_id", input.BotID, "run_id", runID)
	return s.Get(persist, id)
}

func activeTerminalRun(ctx context.Context, tx *sql.Tx, run *domain.Run) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=? AND bot_id=? AND status='running')`, run.ID, run.BotID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return domain.NewError(409, "RUN_NOT_ACTIVE", "Terminal actions require an active run owned by this bot.")
	}
	return nil
}

func (s *Terminals) updateStatus(ctx context.Context, id, status string, exitCode *int, code string) error {
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var finished any
		if status != "running" {
			finished = time.Now().UnixMilli()
		}
		if err := store.RequireChanged(tx.ExecContext(ctx, `UPDATE terminal_sessions SET status=?,exit_code=?,error_code=NULLIF(?,''),finished_at=? WHERE id=?`, status, exitCode, code, finished, id)); err != nil {
			return err
		}
		_, err := bus.Append(ctx, tx, "terminal.updated", "", map[string]string{"terminal_id": id, "status": status})
		return err
	})
	if err == nil {
		s.events.Notify()
	}
	return err
}

func (s *Terminals) watch(id string, entry *terminalEntry) {
	defer s.wg.Done()
	waitErr := entry.session.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), terminalPersistenceTimeout)
	defer cancel()
	err := entry.acquire(ctx)
	if err == nil {
		defer entry.release()
		var terminal Terminal
		if !entry.closed {
			terminal, err = s.Get(ctx, id)
		}
		if !entry.closed && err == nil && terminal.Status != "closed" && terminal.Status != "closing" {
			var output term.SessionOutput
			output, err = entry.session.Read(ctx, 0, 4, false)
			if err == nil {
				err = s.updateStatus(ctx, id, "exited", &output.ExitCode, output.ErrorCode)
			}
		}
	}
	if failure := errors.Join(waitErr, err); failure != nil {
		entry.failureMu.Lock()
		entry.failure = failure
		entry.failureMu.Unlock()
		s.logger.Error("终端退出或状态保存失败", "terminal_id", id)
	}
}

// Shutdown 先停止清理调度，再关闭所有进程并等待退出状态保存完成。
func (s *Terminals) Shutdown(ctx context.Context) error {
	s.stop()
	s.mu.Lock()
	s.mu.Unlock()
	err := s.manager.Close(ctx)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, entry := range s.entries {
			err = errors.Join(err, entry.check())
		}
		return err
	}
}
