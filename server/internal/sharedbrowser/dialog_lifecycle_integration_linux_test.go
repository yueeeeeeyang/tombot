//go:build linux && browserintegration

package sharedbrowser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestSharedChromeSequentialDialogLifecycle(t *testing.T) {
	options, access, sessions := testOptions()
	options.CallTimeout, options.DialogTimeout = 2*time.Second, 3*time.Second
	directory, err := os.MkdirTemp("/workspace", "dialog-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chown(directory, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := io.WriteString(w, `<!doctype html><meta charset="utf-8"><button style="position:absolute;left:10px;top:10px;width:200px;height:50px" onclick="document.body.dataset.answer=prompt('请输入内容','默认值')">打开输入框</button>`); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	live := launchTestBrowser(t, filepath.Join(directory, "profile"), options)
	service := live.service
	open := func(index int) Address {
		t.Helper()
		address := Address{Session: sessions[index], WindowID: ids.New()}
		if _, err := service.Open(t.Context(), address); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Execute(t.Context(), address, Command{Sequence: 1, Kind: "navigate", URL: server.URL}); err != nil {
			t.Fatal(err)
		}
		waitValue(t, service, address, `document.readyState`, "complete")
		return address
	}
	address, other := open(0), open(1)
	command := Command{Sequence: 2, Kind: "click", X: 60, Y: 30}
	call, cancel := context.WithCancel(t.Context())
	receipt, err := service.Execute(call, address, command)
	cancel()
	if err != nil || receipt.CommandState != "waiting_dialog" {
		t.Fatal("顺序调用没有返回等待状态", err)
	}
	// 等待超过普通调用预算，原请求取消也不能取消已明确接收的对话框等待。
	time.Sleep(options.CallTimeout + 100*time.Millisecond)
	if duplicate, err := service.Execute(t.Context(), address, command); err != nil || duplicate.CommandState != "waiting_dialog" || duplicate.Dialog.ID != receipt.Dialog.ID {
		t.Fatal("等待动作被原请求或执行预算取消", err)
	}
	if err := service.RespondDialog(t.Context(), address, DialogReply{ID: receipt.Dialog.ID, Accept: true, Text: "顺序答复"}); err != nil {
		t.Fatal(err)
	}
	waitCommandState(t, service, address, command.Sequence, "completed")
	waitValue(t, service, address, `document.body.dataset.answer`, "顺序答复")
	command.Sequence++
	receipt, err = service.Execute(t.Context(), address, command)
	if err != nil || receipt.CommandState != "waiting_dialog" {
		t.Fatal("第二次对话框无法触发", err)
	}
	waitCommandState(t, service, address, command.Sequence, "unknown")
	if _, err := service.Execute(t.Context(), address, command); !errors.Is(err, ErrUnknown) {
		t.Fatal("超时动作可以重放", err)
	}
	if err := service.RespondDialog(t.Context(), address, DialogReply{ID: receipt.Dialog.ID, Accept: true}); !errors.Is(err, ErrUnknown) {
		t.Fatal("超时后仍答复了对话框", err)
	}
	if err := service.CloseWindow(t.Context(), address); err != nil {
		t.Fatal("未知窗口无法回收", err)
	}
	address = open(0)
	command.Sequence = 2
	if _, err := service.Execute(t.Context(), address, command); err != nil {
		t.Fatal(err)
	}
	access.revoke(sessions[0])
	if err := service.ReleaseSession(t.Context(), sessions[0]); err != nil {
		t.Fatal("撤销不能回收等待中的动作", err)
	}
	if _, err := service.Screenshot(t.Context(), other); err != nil {
		t.Fatal("单 Bot 撤销影响另一 Bot", err)
	}
}
