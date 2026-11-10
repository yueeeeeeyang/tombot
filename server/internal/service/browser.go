package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

// BrowserEngine 将浏览器进程与业务状态分开，持久化或审计失败时业务层停止后续动作。
type BrowserEngine interface {
	Restore(context.Context, []browser.Tab) error
	Open(context.Context, browser.Tab) (browser.Tab, error)
	List(context.Context, string) ([]browser.Tab, error)
	Inspect(context.Context, func([]browser.Tab) error) error
	Status(context.Context) (browser.Status, error)
	Changed() <-chan struct{}
	Restart(context.Context) error
	Close(context.Context) error
	CloseTab(context.Context, browser.Selection) error
	Navigate(context.Context, browser.Selection, string) (browser.Tab, error)
	Snapshot(context.Context, browser.Selection) (browser.Snapshot, error)
	CaptureFrame(context.Context, browser.Selection, int) (browser.ScreenFrame, error)
	Click(context.Context, browser.Selection, string) error
	ClickPoint(context.Context, browser.Selection, float64, float64) error
	InputText(context.Context, browser.Selection, string) error
	Type(context.Context, browser.Selection, string, string, bool) error
	Key(context.Context, browser.Selection, string) error
	Scroll(context.Context, browser.Selection, float64, float64) error
	Download(context.Context, browser.Selection, browser.DownloadInput, func(context.Context, string, io.Reader) error) error
}

type BrowserDependencies struct {
	DB      *sql.DB
	Bots    *Bots
	Events  *bus.Bus
	Logger  *slog.Logger
	Options browser.Options
	Factory func(context.Context, browser.Options) (BrowserEngine, error)
}

type BrowserTab struct {
	ID                string                     `json:"id"`
	BotID             string                     `json:"bot_id,omitempty"`
	URL               string                     `json:"url"`
	Title             string                     `json:"title"`
	Controller        string                     `json:"controller"`
	Revision          uint64                     `json:"revision"`
	PageRevision      uint64                     `json:"page_revision"`
	Status            string                     `json:"status"`
	ErrorCode         string                     `json:"error_code,omitempty"`
	BlockedNavigation *browser.BlockedNavigation `json:"blocked_navigation,omitempty"`
	CreatedAt         int64                      `json:"created_at"`
	UpdatedAt         int64                      `json:"updated_at"`
}

func (tab BrowserTab) owner() string {
	if tab.BotID == "" {
		return "user"
	}
	return tab.BotID
}
func (tab BrowserTab) runtime() browser.Tab {
	return browser.Tab{ID: tab.ID, BotID: tab.owner(), URL: tab.URL, Title: tab.Title, Revision: tab.PageRevision}
}
func (tab BrowserTab) selection() browser.Selection {
	return browser.Selection{ID: tab.ID, Owner: tab.owner(), Revision: tab.PageRevision}
}

type Browser struct {
	db                   *sql.DB
	bots                 *Bots
	events               *bus.Bus
	logger               *slog.Logger
	engine               BrowserEngine
	root                 context.Context
	stop                 context.CancelFunc
	gate                 chan struct{}
	wg                   sync.WaitGroup
	failure              error
	closed               bool
	availabilityCheck    chan struct{}
	reportedAvailability browserAvailability
}

// NewBrowser 装载有界标签，标记中断操作，但在调用 Start 前不启动 Chromium。
func NewBrowser(ctx context.Context, dependencies BrowserDependencies) (*Browser, error) {
	if dependencies.DB == nil || dependencies.Bots == nil || dependencies.Events == nil || dependencies.Logger == nil {
		return nil, errors.New("浏览器服务依赖不完整")
	}
	root, stop := context.WithCancel(ctx)
	s := &Browser{db: dependencies.DB, bots: dependencies.Bots, events: dependencies.Events, logger: dependencies.Logger, root: root, stop: stop, gate: make(chan struct{}, 1), availabilityCheck: make(chan struct{}, 1)}
	if err := s.recoverRecords(ctx); err != nil {
		stop()
		return nil, err
	}
	options := dependencies.Options
	options.Logger = dependencies.Logger
	options.PersistPopup = s.persistPopup
	options.AuthorizeNavigation = s.authorizeNavigation
	factory := dependencies.Factory
	if factory == nil {
		factory = func(ctx context.Context, options browser.Options) (BrowserEngine, error) {
			return browser.New(ctx, options)
		}
	}
	// 进程由 Shutdown 有序关闭，避免应用取消信号抢先终止 Chromium，来不及刷写登录态。
	engine, err := factory(context.WithoutCancel(ctx), options)
	if err != nil {
		stop()
		return nil, err
	}
	s.engine = engine
	tabs, err := s.List(ctx, "")
	if err == nil {
		restored := make([]browser.Tab, 0, len(tabs))
		for _, tab := range tabs {
			if tab.Status == "active" {
				restored = append(restored, tab.runtime())
			}
		}
		err = engine.Restore(ctx, restored)
	}
	if err != nil {
		stop()
		cleanup, cancel := context.WithTimeout(context.Background(), browser.OperationTimeout)
		defer cancel()
		return nil, errors.Join(err, engine.Close(cleanup))
	}
	s.wg.Add(1)
	go s.watch()
	return s, nil
}

func (s *Browser) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.gate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		s.release()
		return err
	}
	return nil
}
func (s *Browser) release() { <-s.gate }
func (s *Browser) healthy() error {
	if s.closed {
		return domain.NewError(503, "BROWSER_UNAVAILABLE", "The browser service is stopping.")
	}
	if s.failure != nil {
		return domain.NewError(503, "BROWSER_STATE_FAILED", "Browser state could not be saved. Inspect the tabs and recover the browser before continuing.")
	}
	return nil
}
func (s *Browser) failed(err error) error {
	if err == nil {
		return nil
	}
	s.failure = err
	s.checkAvailability()
	s.logger.Error("浏览器状态保存失败，后续动作已停止", "component", "browser")
	return errors.Join(domain.NewError(503, "BROWSER_STATE_FAILED", "Browser state could not be saved. Do not repeat the action automatically."), err)
}

// Start 只在依赖引导完成后调用；浏览器启动失败不影响其他业务服务。
func (s *Browser) Start(parent context.Context) error {
	defer s.checkAvailability()
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return err
	}
	if err := s.engine.Restart(ctx); err != nil {
		return err
	}
	return s.sync(ctx)
}

type BrowserState struct {
	browser.Status
	Available  bool   `json:"available"`
	StateError string `json:"state_error,omitempty"`
}

func (s *Browser) Status(parent context.Context) (BrowserState, error) {
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserState{}, err
	}
	defer s.release()
	return s.statusLocked(ctx)
}

// statusLocked 由服务门禁保护，将运行时状态和持久化失败合并成客户端可以使用的一个判断。
func (s *Browser) statusLocked(ctx context.Context) (BrowserState, error) {
	status, err := s.engine.Status(ctx)
	if err != nil {
		return BrowserState{}, err
	}
	result := BrowserState{Status: status}
	result.Available = !s.closed && s.failure == nil && (status.State == "stopped" || status.State == "running")
	if s.failure != nil {
		result.StateError = "BROWSER_STATE_FAILED"
	}
	return result, nil
}

type browserAvailability struct {
	State      string `json:"state"`
	Available  bool   `json:"available"`
	StateError string `json:"state_error,omitempty"`
}

// checkAvailability 可以在业务事务内调用；由观察者在门禁释放后读取并提交通知，避免嵌套写事务。
func (s *Browser) checkAvailability() {
	select {
	case s.availabilityCheck <- struct{}{}:
	default:
	}
}

func (s *Browser) publishAvailability(ctx context.Context) error {
	status, err := s.statusLocked(ctx)
	if err != nil {
		return err
	}
	current := browserAvailability{State: status.State, Available: status.Available, StateError: status.StateError}
	if current == s.reportedAvailability {
		return nil
	}
	if _, err = s.events.Publish(ctx, "browser.availability", "", current); err != nil {
		return err
	}
	s.reportedAvailability = current
	return nil
}

type rowScanner interface{ Scan(...any) error }

const browserColumns = "id,COALESCE(bot_id,''),url,title,controller,revision,page_revision,status,error_code,created_at,updated_at,blocked_url,blocked_method,blocked_code"

func scanBrowserTab(row rowScanner) (BrowserTab, error) {
	var tab BrowserTab
	var blocked browser.BlockedNavigation
	err := row.Scan(&tab.ID, &tab.BotID, &tab.URL, &tab.Title, &tab.Controller, &tab.Revision, &tab.PageRevision, &tab.Status, &tab.ErrorCode, &tab.CreatedAt, &tab.UpdatedAt, &blocked.URL, &blocked.Method, &blocked.Code)
	if errors.Is(err, sql.ErrNoRows) {
		return tab, domain.NewError(404, "BROWSER_TAB_NOT_FOUND", "The browser tab does not exist.")
	}
	if blocked.Code != "" {
		tab.BlockedNavigation = &blocked
	}
	return tab, err
}
func (s *Browser) Get(ctx context.Context, id string) (BrowserTab, error) {
	return scanBrowserTab(s.db.QueryRowContext(ctx, "SELECT "+browserColumns+" FROM browser_tabs WHERE id=?", id))
}

// List 返回所有有效记录，包括需要人工检查的失败状态；超过运行时上限视为损坏，不能无界加载。
func (s *Browser) List(ctx context.Context, botID string) ([]BrowserTab, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+browserColumns+" FROM browser_tabs WHERE (?='' OR bot_id=?) ORDER BY id LIMIT ?", botID, botID, browser.MaxTabs+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tabs := []BrowserTab{}
	for rows.Next() {
		tab, err := scanBrowserTab(rows)
		if err != nil {
			return nil, err
		}
		tabs = append(tabs, tab)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(tabs) > browser.MaxTabs {
		return nil, domain.NewError(503, "BROWSER_STATE_FAILED", "The saved browser tabs exceed capacity.")
	}
	return tabs, nil
}

func (s *Browser) recoverRecords(ctx context.Context) error {
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM browser_tabs WHERE status IN('opening','acting','closing')").Scan(&count); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM browser_tabs WHERE status='closing'"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='failed',error_code='BROWSER_INTERRUPTED',revision=revision+1,updated_at=? WHERE status IN('opening','acting')", time.Now().UnixMilli()); err != nil {
			return err
		}
		if count > 0 {
			return store.Audit(ctx, tx, "system", "browser.recover", "browser", map[string]int{"interrupted": count})
		}
		return nil
	})
}

func (s *Browser) publish(ctx context.Context, tx *sql.Tx, id, botID string) error {
	_, err := bus.Append(ctx, tx, "browser.updated", "", map[string]string{"tab_id": id, "bot_id": botID})
	return err
}

func (s *Browser) insert(ctx context.Context, tx *sql.Tx, tab BrowserTab) error {
	var total, owned int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(COALESCE(bot_id,'')=?),0) FROM browser_tabs", tab.BotID).Scan(&total, &owned); err != nil {
		return err
	}
	if total >= browser.MaxTabs || owned >= browser.MaxOwnerTabs {
		return domain.NewError(409, "BROWSER_TAB_LIMIT", "Close a browser tab before opening another one.")
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO browser_tabs(id,bot_id,url,title,controller,revision,page_revision,status,error_code,created_at,updated_at) VALUES(?,NULLIF(?,''),?,?,?,?,?,?,?,?,?)", tab.ID, tab.BotID, tab.URL, tab.Title, tab.Controller, tab.Revision, tab.PageRevision, tab.Status, tab.ErrorCode, tab.CreatedAt, tab.UpdatedAt)
	return store.ConstraintError(err)
}

// persistPopup 只访问数据库，不反向获取服务门禁，避免浏览器回调与业务命令互锁。
func (s *Browser) persistPopup(ctx context.Context, parentID string, runtime browser.Tab) error {
	return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		parent, err := scanBrowserTab(tx.QueryRowContext(ctx, "SELECT "+browserColumns+" FROM browser_tabs WHERE id=?", parentID))
		if err != nil {
			return err
		}
		if parent.Status != "active" && parent.Status != "acting" && parent.Status != "opening" {
			return domain.NewError(409, "BROWSER_TAB_CHANGED", "The opener is no longer active.")
		}
		if parent.owner() != runtime.BotID {
			return domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "The popup owner does not match its opener.")
		}
		address := runtime.URL
		if address == "" {
			address = "about:blank"
		}
		if address != "about:blank" {
			if _, err = browser.NormalizeURL(address); err != nil {
				return err
			}
		}
		now := time.Now().UnixMilli()
		tab := BrowserTab{ID: runtime.ID, BotID: parent.BotID, URL: address, Title: runtime.Title, Controller: parent.Controller, Revision: 1, PageRevision: runtime.Revision, Status: "active", CreatedAt: now, UpdatedAt: now}
		if err = s.insert(ctx, tx, tab); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO browser_origin_permissions(tab_id,origin,created_at) SELECT ?,origin,? FROM browser_origin_permissions WHERE tab_id=?", tab.ID, now, parentID); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, "system", "browser.popup", tab.ID, map[string]string{"parent_id": parentID, "bot_id": tab.BotID}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tab.ID, tab.BotID)
	})
}

func (s *Browser) sync(ctx context.Context) error {
	return s.engine.Inspect(ctx, func(tabs []browser.Tab) error { return s.syncTabs(ctx, tabs) })
}

func (s *Browser) syncTabs(ctx context.Context, runtimeTabs []browser.Tab) error {
	stored, err := s.List(ctx, "")
	if err != nil {
		return s.failed(err)
	}
	records := map[string]BrowserTab{}
	for _, tab := range stored {
		records[tab.ID] = tab
	}
	present := map[string]bool{}
	changed := false
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		for _, runtime := range runtimeTabs {
			present[runtime.ID] = true
			tab, ok := records[runtime.ID]
			if !ok || tab.owner() != runtime.BotID {
				return domain.NewError(503, "BROWSER_STATE_FAILED", "The browser contains an unrecorded tab.")
			}
			if tab.Status != "active" {
				continue
			}
			runtime = s.persistable(tab, runtime)
			// 地址元数据可能晚于导航版本到达；已确认恢复为 Web 地址时不能保留过渡期的地址错误。
			if runtime.ErrorCode == "" && tab.ErrorCode != "BROWSER_UNAVAILABLE" && tab.ErrorCode != "BROWSER_RESTORE_FAILED" && tab.ErrorCode != "BROWSER_URL_UNSUPPORTED" && tab.PageRevision == runtime.Revision {
				runtime.ErrorCode = tab.ErrorCode
			}
			if tab.URL == runtime.URL && tab.Title == runtime.Title && tab.PageRevision == runtime.Revision && tab.ErrorCode == runtime.ErrorCode && sameBlockedBrowserNavigation(tab.BlockedNavigation, runtime.BlockedNavigation) {
				continue
			}
			blockedURL, blockedMethod, blockedCode := blockedBrowserFields(runtime.BlockedNavigation)
			if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET url=?,title=?,page_revision=?,error_code=?,updated_at=?,blocked_url=?,blocked_method=?,blocked_code=? WHERE id=?", runtime.URL, runtime.Title, runtime.Revision, runtime.ErrorCode, time.Now().UnixMilli(), blockedURL, blockedMethod, blockedCode, tab.ID); err != nil {
				return err
			}
			if err := s.publish(ctx, tx, tab.ID, tab.BotID); err != nil {
				return err
			}
			changed = true
		}
		for _, tab := range stored {
			if !present[tab.ID] && (tab.Status == "active" || tab.Status == "closing") {
				if _, err := tx.ExecContext(ctx, "DELETE FROM browser_tabs WHERE id=?", tab.ID); err != nil {
					return err
				}
				if err := store.Audit(ctx, tx, "system", "browser.tab.closed", tab.ID, map[string]string{"bot_id": tab.BotID}); err != nil {
					return err
				}
				if err := s.publish(ctx, tx, tab.ID, tab.BotID); err != nil {
					return err
				}
				changed = true
			}
		}
		return nil
	})
	if err != nil {
		return s.failed(err)
	}
	if changed {
		s.events.Notify()
	}
	return nil
}

// persistable 不保存 Chrome 内部错误页或非 Web 地址，避免重启时恢复不受支持的协议。
func (s *Browser) persistable(previous BrowserTab, current browser.Tab) browser.Tab {
	if current.URL != "about:blank" {
		address, err := browser.NormalizeURL(current.URL)
		if err != nil {
			current.URL = previous.URL
			current.ErrorCode = "BROWSER_URL_UNSUPPORTED"
			if current.BlockedNavigation != nil {
				current.ErrorCode = current.BlockedNavigation.Code
			}
		} else {
			current.URL = address
		}
	}
	return current
}

func (s *Browser) watch() {
	defer s.wg.Done()
	for {
		var syncTabs bool
		select {
		case <-s.root.Done():
			return
		case <-s.engine.Changed():
			syncTabs = true
		case <-s.availabilityCheck:
		}
		ctx, cancel := context.WithTimeout(s.root, browser.OperationTimeout)
		if err := s.acquire(ctx); err == nil {
			if syncTabs && s.failure == nil && !s.closed {
				if err = s.sync(ctx); err != nil && ctx.Err() == nil {
					s.logger.Warn("浏览器标签同步失败", "component", "browser")
				}
			}
			if err := s.publishAvailability(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("浏览器能力通知失败，客户端将通过状态查询重新核对", "component", "browser")
			}
			s.release()
		}
		cancel()
	}
}

// Recover 先确认当前进程状态可持久化，再尝试恢复进程；不会重试未确认的页面动作。
func (s *Browser) Recover(parent context.Context, actor string) error {
	defer s.checkAvailability()
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.closed {
		return s.healthy()
	}
	if err := s.sync(ctx); err != nil {
		return err
	}
	if err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='failed',error_code='BROWSER_INTERRUPTED',revision=revision+1,updated_at=? WHERE status IN('opening','acting','closing')", time.Now().UnixMilli()); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "browser.restart", "browser", map[string]string{}); err != nil {
			return err
		}
		return s.publish(ctx, tx, "", "")
	}); err != nil {
		return s.failed(err)
	}
	s.failure = nil
	s.events.Notify()
	if err := s.engine.Restart(ctx); err != nil {
		return err
	}
	return s.sync(ctx)
}

func (s *Browser) Shutdown(ctx context.Context) error {
	s.stop()
	s.wg.Wait()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.sync(ctx)
	return errors.Join(err, s.engine.Close(ctx))
}

type BrowserOpenInput struct {
	BotID string `json:"bot_id"`
	URL   string `json:"url"`
}

// Open 先提交标签身份与审计再导航；失败记录保留，用户可检查或关闭，不自动重放请求。
func (s *Browser) Open(parent context.Context, input BrowserOpenInput, run *domain.Run, actor string) (BrowserTab, error) {
	address, err := browser.NormalizeURL(input.URL)
	if err != nil {
		return BrowserTab{}, err
	}
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err = s.acquire(ctx); err != nil {
		return BrowserTab{}, err
	}
	defer s.release()
	if err = s.healthy(); err != nil {
		return BrowserTab{}, err
	}
	controller := "user"
	if run != nil {
		if input.BotID != "" && input.BotID != run.BotID {
			return BrowserTab{}, domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "A bot may only open its own tabs.")
		}
		input.BotID = run.BotID
		controller = "bot"
		if err = s.validRun(ctx, *run); err != nil {
			return BrowserTab{}, err
		}
	}
	if input.BotID != "" {
		if _, err = s.bots.Get(ctx, input.BotID); err != nil {
			return BrowserTab{}, err
		}
	}
	now := time.Now().UnixMilli()
	tab := BrowserTab{ID: ids.New(), BotID: input.BotID, URL: address, Controller: controller, Revision: 1, PageRevision: 1, Status: "opening", CreatedAt: now, UpdatedAt: now}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := s.insert(ctx, tx, tab); err != nil {
			return err
		}
		if run != nil {
			if err := grantBrowserOrigin(ctx, tx, tab, address, actor); err != nil {
				return err
			}
		}
		return store.Audit(ctx, tx, actor, "browser.open.start", tab.ID, map[string]string{"bot_id": tab.BotID})
	})
	if err != nil {
		return BrowserTab{}, err
	}
	runtime, operationErr := s.engine.Open(ctx, tab.runtime())
	persist, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	status, code := "active", ""
	if operationErr != nil {
		code = browserFailure(operationErr)
		if runtime.ID == "" {
			status = "failed"
			runtime = tab.runtime()
		}
	}
	runtime = s.persistable(tab, runtime)
	if code == "" {
		code = runtime.ErrorCode
	}
	err = store.Transact(persist, s.db, func(tx *sql.Tx) error {
		blockedURL, blockedMethod, blockedCode := blockedBrowserFields(runtime.BlockedNavigation)
		if _, err := tx.ExecContext(persist, "UPDATE browser_tabs SET url=?,title=?,page_revision=?,status=?,error_code=?,updated_at=?,blocked_url=?,blocked_method=?,blocked_code=? WHERE id=?", runtime.URL, runtime.Title, runtime.Revision, status, code, time.Now().UnixMilli(), blockedURL, blockedMethod, blockedCode, tab.ID); err != nil {
			return err
		}
		if err := store.Audit(persist, tx, actor, "browser.open.complete", tab.ID, map[string]string{"bot_id": tab.BotID, "error_code": code}); err != nil {
			return err
		}
		return s.publish(persist, tx, tab.ID, tab.BotID)
	})
	if err != nil {
		return tab, errors.Join(s.failed(err), operationErr)
	}
	s.events.Notify()
	tab, err = s.Get(persist, tab.ID)
	return tab, errors.Join(operationErr, err)
}

func browserFailure(err error) string {
	var public *domain.Error
	if errors.As(err, &public) {
		return public.Code
	}
	if errors.Is(err, context.Canceled) {
		return "BROWSER_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "BROWSER_TIMEOUT"
	}
	return "BROWSER_OPERATION_FAILED"
}

func (s *Browser) validRun(ctx context.Context, run domain.Run) error {
	var active bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id=? AND bot_id=? AND status='running')", run.ID, run.BotID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return domain.NewError(409, "RUN_NOT_ACTIVE", "The browser action requires an active run.")
	}
	return nil
}
