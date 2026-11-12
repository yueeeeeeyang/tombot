package sharedbrowser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
)

func TestDialogOwnershipSequenceAndValidation(t *testing.T) {
	options, _, sessions := testOptions()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	item := &entry{session: sessions[0], window: Window{ID: "window", State: "ready"}, life: root, dialogGate: make(chan struct{}, 1)}
	service := &Service{options: options, root: root, stop: cancel, windows: map[string]*entry{"window": item}}
	address := Address{Session: sessions[0], WindowID: "window"}
	service.observeDialog(item, &page.EventJavascriptDialogOpening{Type: page.DialogTypePrompt, Message: "请输入", DefaultPrompt: "默认内容"})
	if item.window.Dialog.ID != 1 || item.window.Dialog.DefaultPrompt != "默认内容" {
		t.Fatal("对话框没有归入所属窗口")
	}
	for _, reply := range []DialogReply{{}, {ID: 1, Text: "未接受却输入"}, {ID: 1, Accept: true, Text: strings.Repeat("x", 4097)}, {ID: 1, Accept: true, Text: "\x00"}} {
		if err := service.RespondDialog(context.Background(), address, reply); !errors.Is(err, ErrInvalid) {
			t.Fatal("非法对话框答复通过", err)
		}
	}
	foreign := address
	foreign.Session = sessions[1]
	if err := service.RespondDialog(context.Background(), foreign, DialogReply{ID: 1}); !errors.Is(err, ErrDenied) {
		t.Fatal("对话框越权没有拒绝", err)
	}
	service.observeDialog(item, &page.EventJavascriptDialogClosed{})
	service.observeDialog(item, &page.EventJavascriptDialogOpening{Type: page.DialogTypeConfirm, Message: "请确认"})
	if item.window.Dialog.ID != 2 {
		t.Fatal("不同对话框复用同一序号")
	}
	if err := service.RespondDialog(context.Background(), address, DialogReply{ID: 1}); !errors.Is(err, ErrConflict) {
		t.Fatal("旧答复覆盖新对话框", err)
	}
	if err := service.RespondDialog(context.Background(), address, DialogReply{ID: 2, Accept: true, Text: "多余输入"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("非输入框接受文本", err)
	}
	item.dialogReply, item.dialogError = DialogReply{ID: 2, Accept: true}, ErrUnknown
	if err := service.RespondDialog(context.Background(), address, item.dialogReply); !errors.Is(err, ErrUnknown) {
		t.Fatal("未知对话框答复被再次发送", err)
	}
	service.observeDialog(item, &page.EventJavascriptDialogOpening{Type: page.DialogTypeAlert})
	if root.Err() == nil {
		t.Fatal("重叠对话框没有封闭未知状态")
	}
}
