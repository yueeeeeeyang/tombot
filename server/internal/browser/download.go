package browser

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

const MaxDownloadBytes = 32 << 20
const downloadTrackingLimit = 8
const downloadCleanupTimeout = 5 * time.Second

type DownloadInput struct {
	URL string `json:"url,omitempty"`
	Ref string `json:"ref,omitempty"`
}

// Validate 要求地址和新鲜引用二选一；不允许隐式重放先前的点击或提交。
func (input DownloadInput) Validate() error {
	if (input.URL == "") == (input.Ref == "") || len(input.Ref) > 32 {
		return invalid("Download requires exactly one HTTP(S) URL or current element reference.")
	}
	if input.URL != "" {
		_, err := NormalizeURL(input.URL)
		return err
	}
	return nil
}

type downloadRecord struct {
	begin    cdpbrowser.EventDownloadWillBegin
	progress cdpbrowser.EventDownloadProgress
}

// downloadWatch 只保存有界下载状态，进度通知合并；协议回调不执行磁盘或 CDP 命令。
type downloadWatch struct {
	mu      sync.Mutex
	records map[string]downloadRecord
	failure error
	wake    chan struct{}
}

func newDownloadWatch() *downloadWatch {
	return &downloadWatch{records: map[string]downloadRecord{}, wake: make(chan struct{}, 1)}
}

func validDownloadGUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (watch *downloadWatch) receive(event any) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	switch value := event.(type) {
	case *cdpbrowser.EventDownloadWillBegin:
		if !validDownloadGUID(value.GUID) || len(watch.records) >= downloadTrackingLimit {
			watch.failure = downloadGuardFailure()
		} else if _, exists := watch.records[value.GUID]; exists {
			watch.failure = downloadGuardFailure()
		} else {
			watch.records[value.GUID] = downloadRecord{begin: *value, progress: cdpbrowser.EventDownloadProgress{GUID: value.GUID, State: cdpbrowser.DownloadProgressStateInProgress}}
		}
	case *cdpbrowser.EventDownloadProgress:
		record, exists := watch.records[value.GUID]
		// 前一次已取消的下载可能还有尾部通知；它们不属于本次接收集合。
		if !exists {
			return
		}
		if math.IsNaN(value.TotalBytes) || math.IsNaN(value.ReceivedBytes) || math.IsInf(value.TotalBytes, 0) || math.IsInf(value.ReceivedBytes, 0) || value.TotalBytes < 0 || value.ReceivedBytes < record.progress.ReceivedBytes {
			watch.failure = downloadGuardFailure()
		}
		if value.State != cdpbrowser.DownloadProgressStateInProgress && value.State != cdpbrowser.DownloadProgressStateCompleted && value.State != cdpbrowser.DownloadProgressStateCanceled {
			watch.failure = downloadGuardFailure()
		}
		record.progress = *value
		watch.records[value.GUID] = record
	default:
		return
	}
	select {
	case watch.wake <- struct{}{}:
	default:
	}
}

func (watch *downloadWatch) snapshot() ([]downloadRecord, error) {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	records := make([]downloadRecord, 0, len(watch.records))
	for _, record := range watch.records {
		records = append(records, record)
	}
	return records, watch.failure
}

func downloadGuardFailure() error {
	return domain.NewError(503, "BROWSER_DOWNLOAD_GUARD_FAILED", "Download protection failed. Inspect the browser and recover it explicitly; do not replay the download.")
}

func (m *Manager) stopUnsafeDownload() {
	m.downloadFailed.Store(true)
	m.options.Logger.Error("浏览器下载无法安全回收，已停止进程", "component", "browser")
	if m.stopBrowser != nil {
		m.stopBrowser()
	}
	m.notify()
}

func (m *Manager) downloadStaging() (*os.Root, string, error) {
	path := filepath.Join(m.options.ProfileDir, "tom-downloads")
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, "", err
	}
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
		return nil, "", errors.New("浏览器下载暂存必须是私有真实目录")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, "", err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(stat, opened) {
		return nil, "", errors.Join(errors.New("浏览器下载暂存目录发生变化"), err, root.Close())
	}
	return root, path, nil
}

// clearDownloadStaging 仅在旧 Chromium 退出后执行；启动不会继续或发布中断下载。
func (m *Manager) clearDownloadStaging() (err error) {
	root, _, err := m.downloadStaging()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(downloadTrackingLimit + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) > downloadTrackingLimit {
		return errors.New("浏览器下载暂存目录超过恢复容量")
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "job-") {
			return errors.New("浏览器下载暂存包含未知文件")
		}
		if _, err := ulid.ParseStrict(strings.TrimPrefix(entry.Name(), "job-")); err != nil {
			return errors.New("浏览器下载暂存名称无效")
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// Download 使用浏览器登录态触发一次下载，完成、撤销接收权限并校验文件后才调用 consume。
// 回调必须同步消费流；失败、取消和重启均不重试请求，临时文件在返回前清理。
func (m *Manager) Download(parent context.Context, selected Selection, input DownloadInput, consume func(context.Context, string, io.Reader) error) error {
	if err := input.Validate(); err != nil {
		return err
	}
	if consume == nil {
		return invalid("A synchronous download consumer is required.")
	}
	return m.withTab(parent, selected, func(ctx context.Context, tab *entry) (err error) {
		defer func() {
			// 非下载响应也可能已经导航；失败结果必须反映实际页面，不能保留旧来源。
			if m.alive() {
				metadata, cancel := context.WithTimeout(tab.ctx, downloadCleanupTimeout)
				err = errors.Join(err, m.metadata(metadata, tab))
				cancel()
			}
		}()
		if input.Ref != "" {
			if _, err = findReference(tab, input.Ref); err != nil {
				return err
			}
		}
		root, path, err := m.downloadStaging()
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, root.Close()) }()
		job := "job-" + ids.New()
		if err = root.Mkdir(job, 0700); err != nil {
			return err
		}
		defer func() {
			if cleanupErr := root.RemoveAll(job); cleanupErr != nil {
				m.stopUnsafeDownload()
				err = errors.Join(err, downloadGuardFailure(), cleanupErr, root.RemoveAll(job))
			}
		}()
		watch := newDownloadWatch()
		listening, stop := context.WithCancel(m.browser)
		defer stop()
		if err = chromedp.ListenBrowser(listening, watch.receive); err != nil {
			return err
		}
		session := chromedp.FromContext(m.browser).Browser
		closed := false
		needsStopLoading := true
		stoppedLoading := false
		expectsDownload := false
		closeDownloads := func() error {
			if closed {
				return nil
			}
			closed = true
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(m.browser), downloadCleanupTimeout)
			defer cancel()
			records, watchErr := watch.snapshot()
			m.options.Logger.Debug("开始回收浏览器下载", "tab_id", tab.ID, "download_job", job, "downloads", len(records), "expects_download", expectsDownload, "navigation_pending", needsStopLoading, "stopped_loading", stoppedLoading)
			if watchErr != nil {
				m.options.Logger.Warn("浏览器下载事件状态异常", "tab_id", tab.ID)
			}
			var stopErr error
			if err != nil && needsStopLoading && !stoppedLoading && len(records) == 0 {
				// 导航取消可能早于下载事件，届时还没有可交给 CancelDownload 的标识。
				_, stopErr = cdp.Call(cleanup, chromedp.FromContext(tab.ctx).Target, page.StopLoading, cdp.Empty{})
			}
			if expectsDownload {
				// Page 与 Browser 会话的事件可交错；导航已交给下载器时须等到标识再取消。
				for {
					records, watchErr := watch.snapshot()
					if len(records) != 0 || watchErr != nil {
						stopErr = errors.Join(stopErr, watchErr)
						break
					}
					select {
					case <-watch.wake:
					case <-cleanup.Done():
						stopErr = errors.Join(stopErr, cleanup.Err())
					}
					if cleanup.Err() != nil {
						break
					}
				}
			}
			closeErr := errors.Join(stopErr, closeBrowserDownloads(cleanup, session, watch, func(ctx context.Context, guid string) error {
				return waitDownloadFile(ctx, root, job, guid)
			}))
			if closeErr != nil {
				m.stopUnsafeDownload()
				return errors.Join(downloadGuardFailure(), closeErr)
			}
			return nil
		}
		defer func() { err = errors.Join(err, closeDownloads()) }()
		enabled := true
		_, err = cdp.Call(ctx, session, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{Behavior: cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName, DownloadPath: filepath.Join(path, job), EventsEnabled: &enabled})
		if err != nil {
			return err
		}
		tree, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, page.GetFrameTree, cdp.Empty{})
		if err != nil {
			return err
		}
		if tree.FrameTree == nil || tree.FrameTree.Frame == nil {
			return downloadGuardFailure()
		}
		frameID := tree.FrameTree.Frame.ID
		trigger, cancelTrigger := context.WithTimeout(context.WithoutCancel(ctx), OperationTimeout)
		triggered := make(chan downloadTrigger, 1)
		// 页面命令未返回时也要处理超限和冲突事件，不能让暂存下载等到命令超时才取消。
		go func() { triggered <- m.triggerDownload(trigger, tab, input) }()
		awaiting := true
		defer func() {
			if awaiting {
				// 保留导航结果，避免仅取消 CDP 等待后丢失迟到的下载归属。
				completion, cancel := context.WithTimeout(context.WithoutCancel(m.browser), downloadCleanupTimeout)
				defer cancel()
				_, stopErr := cdp.Call(completion, chromedp.FromContext(tab.ctx).Target, page.StopLoading, cdp.Empty{})
				stoppedLoading = stopErr == nil
				select {
				case outcome := <-triggered:
					needsStopLoading, expectsDownload = outcome.needsStopLoading, outcome.isDownload
					var protocol *cdproto.Error
					alreadyEnded := !outcome.needsStopLoading && errors.As(stopErr, &protocol) && protocol.Code == -32000 && protocol.Message == "Not attached to an active page"
					if alreadyEnded {
						m.options.Logger.Debug("下载导航已结束，旧页面停止命令无需继续", "tab_id", tab.ID)
					}
					if stopErr != nil && !alreadyEnded {
						m.stopUnsafeDownload()
						err = errors.Join(err, downloadGuardFailure(), stopErr)
					}
				case <-completion.Done():
					m.stopUnsafeDownload()
					cancelTrigger()
					<-triggered
					err = errors.Join(err, downloadGuardFailure())
				}
			}
			cancelTrigger()
		}()
		for {
			records, watchErr := watch.snapshot()
			if watchErr != nil {
				return watchErr
			}
			if len(records) > 1 {
				return domain.NewError(409, "BROWSER_DOWNLOAD_CONFLICT", "More than one download started. All downloads were canceled; inspect the page.")
			}
			if len(records) == 1 {
				record := records[0]
				if record.begin.FrameID != frameID {
					return domain.NewError(403, "BROWSER_DOWNLOAD_OWNER", "The download did not originate from the selected main document.")
				}
				if record.progress.TotalBytes > MaxDownloadBytes || record.progress.ReceivedBytes > MaxDownloadBytes {
					return domain.NewError(413, "BROWSER_DOWNLOAD_TOO_LARGE", "Browser downloads are limited to 32 MiB.")
				}
				switch record.progress.State {
				case cdpbrowser.DownloadProgressStateCanceled:
					return domain.NewError(502, "BROWSER_DOWNLOAD_CANCELED", "Chromium canceled the download. Do not retry automatically.")
				case cdpbrowser.DownloadProgressStateCompleted:
					if awaiting {
						break
					}
					if err = closeDownloads(); err != nil {
						return err
					}
					final, finalErr := watch.snapshot()
					if finalErr != nil {
						return finalErr
					}
					if len(final) != 1 {
						return domain.NewError(409, "BROWSER_DOWNLOAD_CONFLICT", "Another download started before completion. No file was published.")
					}
					return consumeDownload(ctx, root, job, record, consume)
				case cdpbrowser.DownloadProgressStateInProgress:
				default:
					return downloadGuardFailure()
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case outcome := <-triggered:
				awaiting = false
				triggered = nil
				needsStopLoading = outcome.needsStopLoading
				expectsDownload = outcome.isDownload
				if outcome.err != nil {
					return outcome.err
				}
			case <-watch.wake:
			}
		}
	})
}

type downloadTrigger struct {
	err              error
	needsStopLoading bool
	isDownload       bool
}

func (m *Manager) triggerDownload(ctx context.Context, tab *entry, input DownloadInput) downloadTrigger {
	if input.Ref != "" {
		return downloadTrigger{err: m.click(ctx, tab, input.Ref), needsStopLoading: true}
	}
	tab.refs = nil
	tab.Revision++
	defer m.notify()
	result, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, page.Navigate, page.NavigateParams{URL: input.URL})
	if err == nil && !result.IsDownload {
		// 浏览器已结束导航，错误页切换时旧页面会话可能失效；无需再向它发送停止命令。
		return downloadTrigger{err: domain.NewError(409, "BROWSER_DOWNLOAD_NOT_STARTED", "The address did not start a download. Inspect the page; do not repeat the request automatically.")}
	}
	return downloadTrigger{err: err, needsStopLoading: true, isDownload: result.IsDownload}
}

func closeBrowserDownloads(ctx context.Context, session cdp.Session, watch *downloadWatch, beforeCancel func(context.Context, string) error) error {
	enabled := true
	if _, err := cdp.Call(ctx, session, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{Behavior: cdpbrowser.SetDownloadBehaviorBehaviorDeny, EventsEnabled: &enabled}); err != nil {
		return err
	}
	canceled := map[string]bool{}
	for {
		records, err := watch.snapshot()
		if err != nil {
			return err
		}
		active := false
		for _, record := range records {
			if record.progress.State == cdpbrowser.DownloadProgressStateCompleted || record.progress.State == cdpbrowser.DownloadProgressStateCanceled {
				continue
			}
			active = true
			if !canceled[record.begin.GUID] {
				if beforeCancel == nil {
					return downloadGuardFailure()
				}
				if err = beforeCancel(ctx, record.begin.GUID); err != nil {
					return err
				}
				if _, err = cdp.Call(ctx, session, cdpbrowser.CancelDownload, cdpbrowser.CancelDownloadParams{GUID: record.begin.GUID}); err != nil {
					return err
				}
				canceled[record.begin.GUID] = true
			}
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-watch.wake:
		}
	}
}

// waitDownloadFile 等待 Chromium 完成文件初始化；提前取消时其异步建目录任务可能晚于清理。
// 这里只轮询明确的本地状态，不重发网络请求；期限内无法确认则由调用方停止浏览器后清理。
func waitDownloadFile(ctx context.Context, root *os.Root, job, guid string) error {
	backoff := 10 * time.Millisecond
	for {
		for _, suffix := range []string{".crdownload", ""} {
			stat, err := root.Lstat(filepath.Join(job, guid+suffix))
			if err == nil {
				if !stat.Mode().IsRegular() {
					return downloadGuardFailure()
				}
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, 100*time.Millisecond)
	}
}

func consumeDownload(ctx context.Context, root *os.Root, job string, record downloadRecord, consume func(context.Context, string, io.Reader) error) (err error) {
	name := filepath.Join(job, record.begin.GUID)
	stat, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > MaxDownloadBytes || stat.Size() != int64(record.progress.ReceivedBytes) {
		return domain.NewError(502, "BROWSER_DOWNLOAD_INVALID", "The completed download has an invalid size or file type.")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(stat, opened) {
		return domain.NewError(409, "BROWSER_DOWNLOAD_INVALID", "The downloaded file changed before it could be saved.")
	}
	return consume(ctx, record.begin.SuggestedFilename, io.LimitReader(file, MaxDownloadBytes+1))
}
