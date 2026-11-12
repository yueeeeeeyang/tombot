package sharedbrowser

import (
	"context"
	"math"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// observeDialog 同步记录页面事件，让已返回的命令不会遗漏等待中的对话框；回调不发 CDP 命令。
func (service *Service) observeDialog(item *entry, event any) {
	switch event := event.(type) {
	case *page.EventJavascriptDialogOpening:
		if !validText(event.Message, 16384) || !validText(event.DefaultPrompt, 4096) {
			service.fail()
			return
		}
		switch event.Type {
		case page.DialogTypeAlert, page.DialogTypeConfirm, page.DialogTypePrompt, page.DialogTypeBeforeunload:
		default:
			service.fail()
			return
		}
		service.mu.Lock()
		if item.dialogSequence == math.MaxInt64 || item.window.Dialog.ID != 0 {
			service.mu.Unlock()
			service.fail()
			return
		}
		item.dialogSequence++
		item.window.Dialog = Dialog{ID: item.dialogSequence, Kind: string(event.Type), Message: event.Message, DefaultPrompt: event.DefaultPrompt}
		if item.active != nil {
			item.window.CommandState = "waiting_dialog"
			item.active.setWaiting(true)
		}
		service.mu.Unlock()
	case *page.EventJavascriptDialogClosed:
		service.mu.Lock()
		item.window.Dialog = Dialog{}
		if item.active != nil {
			item.window.CommandState = "running"
			item.active.setWaiting(false)
		}
		service.mu.Unlock()
	}
}

// RespondDialog 使用独立的窄门禁结束指定对话框，因为触发它的页面动作可能仍在等待。
// 重复末次答复只返回原回执；不接受旧对话框、跨窗口引用或未经确认的答复重放。
func (service *Service) RespondDialog(ctx context.Context, address Address, reply DialogReply) error {
	if reply.ID < 1 || !validText(reply.Text, 4096) || !reply.Accept && reply.Text != "" {
		return ErrInvalid
	}
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	item, err := service.own(bounded, address)
	if err != nil {
		return err
	}
	interrupt := context.AfterFunc(item.life, cancel)
	defer interrupt()
	if err := acquire(bounded, item.dialogGate); err != nil {
		return err
	}
	defer func() { <-item.dialogGate }()
	if item.life.Err() != nil {
		return ErrDenied
	}
	if err := service.authorize(bounded, address.Session); err != nil {
		return err
	}
	if reply.ID == item.dialogReply.ID {
		if reply != item.dialogReply {
			return ErrConflict
		}
		return item.dialogError
	}
	service.mu.Lock()
	dialog, ready := item.window.Dialog, item.window.State == "ready"
	if item.active != nil && item.active.ctx.Err() != nil {
		ready = false
	}
	service.mu.Unlock()
	if !ready || item.dialogError != nil {
		return ErrUnknown
	}
	if dialog.ID != reply.ID || reply.ID < item.dialogReply.ID {
		return ErrConflict
	}
	if dialog.Kind != string(page.DialogTypePrompt) && reply.Text != "" {
		return ErrInvalid
	}
	item.dialogReply, item.dialogError = reply, ErrUnknown
	_, err = cdp.Call(bounded, chromedp.FromContext(item.ctx).Target, page.HandleJavaScriptDialog, page.HandleJavaScriptDialogParams{Accept: reply.Accept, PromptText: reply.Text})
	if err == nil {
		err = service.authorize(bounded, address.Session)
	}
	if err != nil {
		service.mu.Lock()
		item.window.State = "unknown"
		service.mu.Unlock()
		return ErrUnknown
	}
	item.dialogError = nil
	return nil
}
