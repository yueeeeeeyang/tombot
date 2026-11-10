package service

import (
	"context"
	"database/sql"
	"io"
	"time"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/workspace"
)

type BrowserDownloadInput struct {
	BrowserVersion
	browser.DownloadInput
}

type BrowserDownload struct {
	Tab  BrowserTab            `json:"tab"`
	File workspace.WriteResult `json:"file"`
}

type BrowserDownloadSaver func(context.Context, string, string, io.Reader) (workspace.WriteResult, error)

// Download 审批与用户操作绑定双版本，先保存意图，再执行一次浏览器请求并发布文件；失败不会重放。
func (s *Browser) Download(parent context.Context, id string, input BrowserDownloadInput, run *domain.Run, actor string, save BrowserDownloadSaver) (result BrowserDownload, err error) {
	if err = input.Validate(); err != nil {
		return result, err
	}
	if save == nil {
		return result, domain.NewError(500, "BROWSER_DOWNLOAD_STORAGE", "Download storage is unavailable.")
	}
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err = s.acquire(ctx); err != nil {
		return result, err
	}
	defer s.release()
	if err = s.healthy(); err != nil {
		return result, err
	}
	if err = s.sync(ctx); err != nil {
		return result, err
	}
	tab, err := s.selected(ctx, id, run, input.BrowserVersion, true, false)
	if err != nil {
		return result, err
	}
	result.Tab = tab
	if tab.BlockedNavigation != nil {
		return result, domain.NewError(409, "BROWSER_NAVIGATION_BLOCKED", "Inspect the blocked navigation and open an approved page before downloading.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		if run != nil && input.URL != "" {
			if err := grantBrowserOrigin(ctx, tx, tab, input.URL, actor); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE browser_tabs SET status='acting',updated_at=? WHERE id=?", time.Now().UnixMilli(), id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, actor, "browser.download.start", id, map[string]string{"bot_id": tab.BotID})
	})
	if err != nil {
		return result, err
	}
	err = s.engine.Download(ctx, tab.selection(), input.DownloadInput, func(ctx context.Context, name string, source io.Reader) error {
		file, saveErr := save(ctx, tab.BotID, name, source)
		result.File = file
		return saveErr
	})
	result.Tab, err = s.finish(tab, err, actor, "download")
	return result, err
}
