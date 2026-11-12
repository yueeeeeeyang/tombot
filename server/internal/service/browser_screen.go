package service

import (
	"context"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
)

type BrowserFrame struct {
	Tab  BrowserTab
	JPEG []byte
}

// CaptureScreen 将画面与页面版本绑定；同步失败不发送画面，控制权始终来自持久服务状态。
func (s *Browser) CaptureScreen(parent context.Context, id string, quality int) (BrowserFrame, error) {
	return s.captureScreen(parent, id, nil, quality)
}

func (s *Browser) captureScreen(parent context.Context, id string, run *domain.Run, quality int) (BrowserFrame, error) {
	ctx, cancel := context.WithTimeout(parent, browser.OperationTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return BrowserFrame{}, err
	}
	defer s.release()
	if err := s.healthy(); err != nil {
		return BrowserFrame{}, err
	}
	tab, err := s.selected(ctx, id, run, BrowserVersion{}, false, false)
	if err != nil {
		return BrowserFrame{}, err
	}
	selected := tab.selection()
	selected.Revision = 0
	frame, err := s.engine.CaptureFrame(ctx, selected, quality)
	if err != nil {
		return BrowserFrame{}, err
	}
	if err = s.sync(ctx); err != nil {
		return BrowserFrame{}, err
	}
	tab, err = s.Get(ctx, id)
	if err != nil {
		return BrowserFrame{}, err
	}
	runtime := s.persistable(tab, frame.Tab)
	tab.URL, tab.Title, tab.PageRevision = runtime.URL, runtime.Title, runtime.Revision
	tab.ErrorCode, tab.BlockedNavigation = runtime.ErrorCode, runtime.BlockedNavigation
	return BrowserFrame{Tab: tab, JPEG: frame.JPEG}, nil
}
