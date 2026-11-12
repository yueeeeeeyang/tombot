package sharedbrowser

import (
	"context"
	"errors"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/infra/ids"
)

// observe 不在协议读取线程发命令或等待窗口锁；队列满代表归属事件不完整，必须封闭共享服务。
func (service *Service) observe(event any) {
	switch event.(type) {
	case *target.EventTargetCreated, *target.EventTargetDestroyed, *target.EventTargetInfoChanged, *target.EventTargetCrashed:
		select {
		case service.events <- event:
		default:
			service.fail()
		}
	}
}

func (service *Service) runEvents() {
	defer close(service.done)
	defer func() {
		if err := service.connection.Close(); err != nil {
			service.options.Logger.Error("关闭共享浏览器管道失败", "user_id", service.options.UserID, "computer_id", service.options.ComputerID)
		}
	}()
	for {
		select {
		case <-service.root.Done():
			return
		case <-service.browser.LostConnection:
			service.fail()
			return
		case event := <-service.events:
			if err := service.handleEvent(event); err != nil {
				service.fail()
				return
			}
		}
	}
}

func (service *Service) handleEvent(event any) error {
	switch event := event.(type) {
	case *target.EventTargetCreated:
		info := event.TargetInfo
		if info == nil {
			return ErrUnavailable
		}
		if info.Type != "page" {
			return nil
		}
		ctx, cancel := context.WithTimeout(service.root, service.options.CallTimeout)
		defer cancel()
		if err := acquire(ctx, service.creation); err != nil {
			return err
		}
		defer func() { <-service.creation }()
		service.mu.Lock()
		known := service.targets[info.TargetID] != nil || info.TargetID == chromedp.FromContext(service.root).Target.TargetID
		parent := service.targets[info.OpenerID]
		full := false
		if parent != nil {
			full = service.full(parent.session)
		}
		service.mu.Unlock()
		if known {
			return nil
		}
		if parent == nil || parent.life.Err() != nil || full || service.authorize(ctx, parent.session) != nil {
			if parent != nil && full {
				service.mu.Lock()
				parent.window.Notice = "浏览器窗口数量已达上限，已关闭新弹窗"
				service.mu.Unlock()
				service.options.Logger.Warn("共享浏览器弹窗超过容量", "user_id", service.options.UserID, "computer_id", service.options.ComputerID, "bot_id", parent.session.BotID)
			}
			return service.closeTarget(ctx, info.TargetID)
		}
		_, err := service.attach(ctx, Address{Session: parent.session, WindowID: ids.New()}, info.TargetID, parent.window.ID)
		return err
	case *target.EventTargetDestroyed:
		service.mu.Lock()
		item := service.targets[event.TargetID]
		if item != nil {
			delete(service.targets, item.target)
			item.window.State = "closed"
			item.revoke()
		}
		service.mu.Unlock()
		if item != nil {
			item.stop()
			if err := service.releaseUploads(item); err != nil {
				return err
			}
		}
	case *target.EventTargetCrashed:
		service.mu.Lock()
		if item := service.targets[event.TargetID]; item != nil {
			item.window.State = "unknown"
			item.revoke()
		}
		service.mu.Unlock()
	case *target.EventTargetInfoChanged:
		info := event.TargetInfo
		if info == nil || len(info.URL) > 8192 || len(info.Title) > 4096 {
			return ErrUnavailable
		}
		service.mu.Lock()
		if item := service.targets[info.TargetID]; item != nil {
			item.window.URL, item.window.Title = info.URL, info.Title
		}
		service.mu.Unlock()
	}
	return nil
}

func (service *Service) closeUnowned(ctx context.Context) error {
	listed, err := cdp.Call(ctx, service.browser, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return ErrUnavailable
	}
	root := chromedp.FromContext(service.root).Target.TargetID
	for _, info := range listed.TargetInfos {
		if info.Type == "page" && info.TargetID != root {
			if err := service.closeTarget(ctx, info.TargetID); err != nil {
				return err
			}
		}
	}
	return nil
}

// closeTarget 不把 CloseTarget 回包当作已消失；有界查询确认实际目标已移除。
func (service *Service) closeTarget(ctx context.Context, id target.ID) error {
	// 页面可能已自行关闭；即使关闭回包失败，仍以查询确认的目标消失为准。
	_, closeErr := cdp.Call(ctx, service.browser, target.CloseTarget, target.CloseTargetParams{TargetID: id})
	for {
		listed, err := cdp.Call(ctx, service.browser, target.GetTargets, target.GetTargetsParams{})
		if err != nil {
			return ErrUnknown
		}
		found := false
		for _, info := range listed.TargetInfos {
			if info.TargetID == id {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		if closeErr != nil {
			return ErrUnknown
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ErrUnknown, ctx.Err())
		case <-timer.C:
		}
	}
}
