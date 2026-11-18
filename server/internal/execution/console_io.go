package execution

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
)

type consoleStreamRuntime interface {
	ConsoleRuntime
	computer.DesktopStreamRuntime
}
type ConsoleService struct {
	executor *Service
	runtime  consoleStreamRuntime
	logger   *slog.Logger
	mu       sync.Mutex
	closed   bool
	streams  map[*consoleConnection]struct{}
	pending  sync.WaitGroup
}

func NewConsoleService(executor *Service, runtime consoleStreamRuntime, logger *slog.Logger) (*ConsoleService, error) {
	if executor == nil || runtime == nil || logger == nil {
		return nil, ErrInvalidInput
	}
	return &ConsoleService{executor: executor, runtime: runtime, logger: logger, streams: map[*consoleConnection]struct{}{}}, nil
}

// Ready 只启动对应 Bot 的显示服务；不会冻结任务或打开、切换已有浏览器窗口。
func (service *ConsoleService) Ready(ctx context.Context, principal identity.Principal, id string) error {
	if !validID(id) {
		return ErrInvalidInput
	}
	var record consoleRecord
	var session computer.ExecutionSession
	err := service.executor.user(ctx, principal, true, func(tx *sql.Tx) error {
		var err error
		record, err = scanConsole(tx.QueryRowContext(ctx, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND id=$2 AND device_id=$3 AND expires_at>clock_timestamp()`, principal.UserID, id, principal.DeviceID))
		if err != nil {
			return err
		}
		if record.State != "opening" {
			return nil
		}
		if record.sessionID == "" {
			err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.computer_sessions WHERE user_id=$1 AND run_id=$2 AND status='active' AND lease_expires_at>clock_timestamp()`, principal.UserID, record.runID).Scan(&record.sessionID)
			if errors.Is(err, sql.ErrNoRows) {
				record.ID = ""
				return nil
			}
			if err != nil {
				return err
			}
		}
		session, err = consoleSession(ctx, tx, principal.UserID, record)
		return err
	})
	if err != nil || record.ID == "" || record.State != "opening" {
		return err
	}
	start, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	status, err := service.runtime.StartDesktop(start, session)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if status.State == "starting" {
		return nil
	}
	if status.State != "ready" {
		return ErrUnavailable
	}
	return service.executor.user(ctx, principal, true, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tombot.desktop_consoles SET state='viewing',computer_session_id=$4 WHERE user_id=$1 AND id=$2 AND device_id=$3 AND state='opening' AND expires_at>clock_timestamp()`, principal.UserID, id, principal.DeviceID, session.SessionID)
		return err
	})
}

func (service *ConsoleService) authorize(ctx context.Context, principal identity.Principal, id string) (request computer.DesktopStream, err error) {
	if !validID(id) {
		return request, ErrInvalidInput
	}
	err = service.executor.user(ctx, principal, false, func(tx *sql.Tx) error {
		record, err := scanConsole(tx.QueryRowContext(ctx, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND id=$2 AND device_id=$3 AND state IN ('viewing','waiting','pausing','active','resuming') AND expires_at>clock_timestamp()`, principal.UserID, id, principal.DeviceID))
		if err != nil {
			return err
		}
		request.Session, err = consoleSession(ctx, tx, principal.UserID, record)
		request.ViewerID = id
		return err
	})
	return
}

// Stream 不延长观看租约；每秒重查设备、会话与电脑代次，撤销后关闭 RFB 而非继续缓存画面。
func (service *ConsoleService) Stream(ctx context.Context, principal identity.Principal, id string) (net.Conn, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	request, err := service.authorize(bounded, principal, id)
	cancel()
	if err != nil {
		return nil, err
	}
	life, stop := context.WithCancel(ctx)
	connection, err := service.runtime.OpenDesktopStream(life, request)
	if err != nil {
		stop()
		return nil, err
	}
	owned := &consoleConnection{Conn: connection, cancel: stop}
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return nil, errors.Join(ErrUnavailable, owned.Close())
	}
	service.streams[owned] = struct{}{}
	service.pending.Add(1)
	service.mu.Unlock()
	go func() {
		defer service.pending.Done()
		defer func() { service.mu.Lock(); delete(service.streams, owned); service.mu.Unlock() }()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer func() {
			if err := owned.Close(); err != nil {
				service.logger.Warn("关闭桌面观看连接失败", "bot_id", request.Session.BotID, "console_id", id)
			}
		}()
		for {
			select {
			case <-life.Done():
				return
			case <-ticker.C:
			}
			check, end := context.WithTimeout(life, 5*time.Second)
			current, err := service.authorize(check, principal, id)
			end()
			if err != nil || current != request {
				return
			}
		}
	}()
	return owned, nil
}

type consoleConnection struct {
	net.Conn
	cancel  context.CancelFunc
	once    sync.Once
	failure error
}

func (connection *consoleConnection) Close() error {
	connection.once.Do(func() { connection.cancel(); connection.failure = connection.Conn.Close() })
	return connection.failure
}

// Run 使桌面流参与服务退出；等待验证协程停止后再关闭数据库。
func (service *ConsoleService) Run(ctx context.Context) error {
	<-ctx.Done()
	service.mu.Lock()
	service.closed = true
	var failure error
	for stream := range service.streams {
		failure = errors.Join(failure, stream.Close())
	}
	service.mu.Unlock()
	service.pending.Wait()
	return failure
}
