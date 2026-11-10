package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
)

// BrowserVersion 同时绑定控制权和页面版本；用户点击与模型审批都不能跨版本重放。
type BrowserVersion struct {
	Revision     uint64 `json:"revision"`
	PageRevision uint64 `json:"page_revision"`
}

type BrowserAction struct {
	BrowserVersion
	Action string   `json:"action"`
	URL    string   `json:"url,omitempty"`
	Ref    string   `json:"ref,omitempty"`
	Text   string   `json:"text,omitempty"`
	Clear  *bool    `json:"clear,omitempty"`
	Key    string   `json:"key,omitempty"`
	DeltaX float64  `json:"delta_x,omitempty"`
	DeltaY float64  `json:"delta_y,omitempty"`
	X      *float64 `json:"x,omitempty"`
	Y      *float64 `json:"y,omitempty"`
}

func (input BrowserAction) Validate() error {
	switch input.Action {
	case "navigate", "click", "type", "key", "scroll", "click_at", "input":
	default:
		return domain.NewError(400, "INVALID_BROWSER_INPUT", "Use a supported browser action.")
	}
	if input.Action != "navigate" && input.URL != "" || input.Action != "click" && input.Action != "type" && input.Ref != "" || input.Action != "type" && input.Action != "input" && input.Text != "" || input.Action != "type" && input.Clear != nil || input.Action != "key" && input.Key != "" || input.Action != "scroll" && (input.DeltaX != 0 || input.DeltaY != 0) || input.Action != "click_at" && (input.X != nil || input.Y != nil) {
		return domain.NewError(400, "INVALID_BROWSER_INPUT", "The fields do not match the browser action.")
	}
	if input.Action == "navigate" {
		_, err := browser.NormalizeURL(input.URL)
		return err
	}
	if (input.Action == "click" || input.Action == "type") && (input.Ref == "" || len(input.Ref) > 32) {
		return domain.NewError(400, "INVALID_BROWSER_INPUT", "Provide a current element reference.")
	}
	switch input.Action {
	case "type", "input":
		return browser.ValidateText(input.Text)
	case "click_at":
		if input.X == nil || input.Y == nil {
			return domain.NewError(400, "INVALID_BROWSER_INPUT", "Provide both click coordinates.")
		}
		return browser.ValidatePoint(*input.X, *input.Y)
	case "key":
		return browser.ValidateKey(input.Key)
	case "scroll":
		return browser.ValidateScroll(input.DeltaX, input.DeltaY)
	}
	return nil
}

func (s *Browser) selected(ctx context.Context, id string, run *domain.Run, version BrowserVersion, mutating, allowFailed bool) (BrowserTab, error) {
	tab, err := s.Get(ctx, id)
	if err != nil {
		return tab, err
	}
	if run != nil {
		if tab.BotID != run.BotID {
			return tab, domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "This tab belongs to another owner.")
		}
		if err = s.validRun(ctx, *run); err != nil {
			return tab, err
		}
	}
	if tab.Status != "active" && !(allowFailed && tab.Status == "failed") {
		return tab, domain.NewError(409, "BROWSER_TAB_INACTIVE", "Inspect or restore the inactive browser tab before continuing.")
	}
	if mutating {
		if tab.ErrorCode == "BROWSER_URL_UNSUPPORTED" && !allowFailed {
			return tab, domain.NewError(409, "BROWSER_TAB_INACTIVE", "The page left supported web navigation. Restore the last saved web address or close the tab.")
		}
		controller := "user"
		if run != nil {
			controller = "bot"
		}
		if tab.Controller != controller {
			return tab, domain.NewError(409, "BROWSER_CONTROL_REQUIRED", "Take control of the tab before sending input.")
		}
		if version.Revision == 0 || version.PageRevision == 0 || version.Revision != tab.Revision || version.PageRevision != tab.PageRevision {
			return tab, domain.NewError(409, "BROWSER_TAB_CHANGED", "The tab changed. Inspect it before submitting a new action.")
		}
	}
	return tab, nil
}

// Apply 先保存动作意图再执行一次。取消后仍独立保存结果，保存失败则阻断后续动作；审计不记录输入正文或 URL。
func (s *Browser) Apply(parent context.Context, id string, input BrowserAction, run *domain.Run, actor string) (BrowserTab, error) {
	if err := input.Validate(); err != nil {
		return BrowserTab{}, err
	}
	if run != nil && (input.Action == "click_at" || input.Action == "input") {
		return BrowserTab{}, domain.NewError(403, "BROWSER_USER_ACTION_REQUIRED", "Coordinate clicks and focused input are reserved for the authenticated user.")
	}
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserTab{}, err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return BrowserTab{}, err
	}
	if err := s.sync(ctx); err != nil {
		return BrowserTab{}, err
	}
	tab, err := s.selected(ctx, id, run, input.BrowserVersion, true, false)
	if err != nil {
		return tab, err
	}
	if tab.BlockedNavigation != nil && input.Action != "navigate" {
		return tab, domain.NewError(409, "BROWSER_NAVIGATION_BLOCKED", "Inspect the blocked navigation. Open an approved address before sending more input.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if run != nil && input.Action == "navigate" {
			if err := grantBrowserOrigin(ctx, tx, tab, input.URL, actor); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='acting',updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "browser.action.start", id, map[string]any{"bot_id": tab.BotID, "action": input.Action, "input_bytes": len(input.Text)})
	})
	if err != nil {
		return tab, err
	}
	selected := tab.selection()
	switch input.Action {
	case "navigate":
		_, err = s.engine.Navigate(ctx, selected, input.URL)
	case "click":
		err = s.engine.Click(ctx, selected, input.Ref)
	case "click_at":
		err = s.engine.ClickPoint(ctx, selected, *input.X, *input.Y)
	case "input":
		err = s.engine.InputText(ctx, selected, input.Text)
	case "type":
		clear := input.Clear == nil || *input.Clear
		err = s.engine.Type(ctx, selected, input.Ref, input.Text, clear)
	case "key":
		err = s.engine.Key(ctx, selected, input.Key)
	case "scroll":
		err = s.engine.Scroll(ctx, selected, input.DeltaX, input.DeltaY)
	}
	return s.finish(tab, err, actor, "action")
}

func (s *Browser) finish(previous BrowserTab, operationErr error, actor, operation string) (BrowserTab, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.engine.Inspect(ctx, func(tabs []browser.Tab) error {
		current := previous.runtime()
		status, code := "failed", "BROWSER_TAB_LOST"
		for _, tab := range tabs {
			if tab.ID == previous.ID {
				current = s.persistable(previous, tab)
				status = "active"
				code = current.ErrorCode
				break
			}
		}
		if operationErr != nil {
			code = browserFailure(operationErr)
		} else if status == "failed" {
			operationErr = domain.NewError(409, "BROWSER_TAB_LOST", "The tab disappeared while completing the action. Inspect the tab list before continuing.")
		}
		return store.Transact(ctx, s.db, func(tx *sql.Tx) error {
			blockedURL, blockedMethod, blockedCode := blockedBrowserFields(current.BlockedNavigation)
			if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET url=?,title=?,page_revision=?,status=?,error_code=?,updated_at=?,blocked_url=?,blocked_method=?,blocked_code=? WHERE id=?", current.URL, current.Title, current.Revision, status, code, time.Now().UnixMilli(), blockedURL, blockedMethod, blockedCode, previous.ID); err != nil {
				return err
			}
			if err := store.Audit(ctx, tx, actor, "browser."+operation+".complete", previous.ID, map[string]string{"bot_id": previous.BotID, "error_code": code}); err != nil {
				return err
			}
			return s.publish(ctx, tx, previous.ID, previous.BotID)
		})
	})
	if err != nil {
		return previous, errors.Join(s.failed(err), operationErr)
	}
	s.events.Notify()
	tab, err := s.Get(ctx, previous.ID)
	return tab, errors.Join(operationErr, err)
}

// SetController 仅由鉴权用户调用；每次交接递增版本，使交接前待审批的动作失效。
func (s *Browser) SetController(parent context.Context, id, controller string, revision uint64, actor string) (BrowserTab, error) {
	if controller != "user" && controller != "bot" {
		return BrowserTab{}, domain.NewError(400, "INVALID_BROWSER_INPUT", "Controller must be user or bot.")
	}
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserTab{}, err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return BrowserTab{}, err
	}
	// 用户导航可能先于异步元数据通知完成；交接许可必须基于最新运行时记录。
	if err := s.sync(ctx); err != nil {
		return BrowserTab{}, err
	}
	tab, err := s.Get(ctx, id)
	if err != nil {
		return tab, err
	}
	if controller == "bot" && tab.BotID == "" {
		return tab, domain.NewError(409, "BROWSER_NO_BOT", "A user tab cannot be assigned to a bot.")
	}
	if revision == 0 || revision != tab.Revision {
		return tab, domain.NewError(409, "BROWSER_TAB_CHANGED", "The controller changed. Inspect the tab again.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if controller == "bot" && tab.URL != "about:blank" {
			if err := grantBrowserOrigin(ctx, tx, tab, tab.URL, actor); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET controller=?,revision=revision+1,updated_at=? WHERE id=?", controller, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "browser.control", id, map[string]string{"bot_id": tab.BotID, "controller": controller}); err != nil {
			return err
		}
		return s.publish(ctx, tx, id, tab.BotID)
	})
	if err != nil {
		return tab, err
	}
	s.events.Notify()
	return s.Get(ctx, id)
}

type BrowserSnapshot struct {
	Tab       BrowserTab `json:"tab"`
	Text      string     `json:"text"`
	Truncated bool       `json:"truncated"`
}

// Snapshot 可旁观其他控制者，但模型仍须属于当前标签；读取生成新 ref，不保存页面正文。
func (s *Browser) Snapshot(parent context.Context, id string, run *domain.Run) (BrowserSnapshot, error) {
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserSnapshot{}, err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return BrowserSnapshot{}, err
	}
	tab, err := s.selected(ctx, id, run, BrowserVersion{}, false, false)
	if err != nil {
		return BrowserSnapshot{}, err
	}
	selected := tab.selection()
	selected.Revision = 0
	snapshot, operationErr := s.engine.Snapshot(ctx, selected)
	if operationErr != nil {
		return BrowserSnapshot{}, operationErr
	}
	if err = s.sync(ctx); err != nil {
		return BrowserSnapshot{}, err
	}
	tab, err = s.Get(ctx, id)
	// 快照完成后可能立即跳转，返回快照对应版本，旧引用由执行层再次校验。
	tab.URL, tab.Title, tab.PageRevision = snapshot.Tab.URL, snapshot.Tab.Title, snapshot.Tab.Revision
	return BrowserSnapshot{Tab: tab, Text: snapshot.Text, Truncated: snapshot.Truncated}, err
}

// Screenshot 只返回视口 JPEG；不落盘，旁观不改变控制权。
func (s *Browser) Screenshot(parent context.Context, id string, run *domain.Run, quality int) ([]byte, error) {
	frame, err := s.captureScreen(parent, id, run, quality)
	return frame.JPEG, err
}

// CloseTab 可清理失败记录；先保存关闭意图，服务重启时不会重新打开已确认要关闭的标签。
func (s *Browser) CloseTab(parent context.Context, id string, version BrowserVersion, run *domain.Run, actor string) error {
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.closed {
		return s.healthy()
	}
	tab, err := s.Get(ctx, id)
	if isBrowserMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if run != nil {
		if _, err = s.selected(ctx, id, run, version, true, true); err != nil {
			return err
		}
	} else if version.Revision == 0 || version.Revision != tab.Revision {
		return domain.NewError(409, "BROWSER_TAB_CHANGED", "The controller changed. Inspect the tab before closing it.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='closing',updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "browser.close.start", id, map[string]string{"bot_id": tab.BotID})
	})
	if err != nil {
		return err
	}
	selected := tab.selection()
	if run == nil {
		selected.Revision = 0
	}
	operationErr := s.engine.CloseTab(ctx, selected)
	if operationErr != nil && !isBrowserMissing(operationErr) {
		_, err = s.finish(tab, operationErr, actor, "close")
		return err
	}
	persist, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPersist()
	err = store.Transact(persist, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(persist, "DELETE FROM browser_tabs WHERE id=?", id); err != nil {
			return err
		}
		if err := store.Audit(persist, tx, actor, "browser.close.complete", id, map[string]string{"bot_id": tab.BotID}); err != nil {
			return err
		}
		return s.publish(persist, tx, id, tab.BotID)
	})
	if err != nil {
		return s.failed(err)
	}
	s.events.Notify()
	return nil
}

func isBrowserMissing(err error) bool {
	var failure *domain.Error
	return errors.As(err, &failure) && failure.Code == "BROWSER_TAB_NOT_FOUND"
}

// RestoreTab 由用户显式重新打开失败标签；保留身份并递增版本，不重试先前的输入或点击。
func (s *Browser) RestoreTab(parent context.Context, id string, revision uint64, actor string) (BrowserTab, error) {
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserTab{}, err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return BrowserTab{}, err
	}
	tab, err := s.Get(ctx, id)
	if err != nil {
		return tab, err
	}
	if tab.Status != "failed" && tab.ErrorCode != "BROWSER_RESTORE_FAILED" && tab.ErrorCode != "BROWSER_URL_UNSUPPORTED" || revision == 0 || revision != tab.Revision {
		return tab, domain.NewError(409, "BROWSER_TAB_CHANGED", "Only an unchanged failed tab can be restored.")
	}
	if tab.URL == "about:blank" {
		return tab, domain.NewError(409, "BROWSER_TAB_INACTIVE", "Close this blank failed tab and open a web address.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='opening',controller='user',revision=revision+1,updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "browser.restore.start", id, map[string]string{"bot_id": tab.BotID})
	})
	if err != nil {
		return tab, err
	}
	selected := tab.selection()
	selected.Revision = 0
	err = s.engine.CloseTab(ctx, selected)
	if err == nil || isBrowserMissing(err) {
		requested := tab.runtime()
		requested.Revision++
		_, err = s.engine.Open(ctx, requested)
	}
	return s.finish(tab, err, actor, "restore")
}
