//go:build linux && browserintegration

package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type unreadUpload struct{ reads int }

func (reader *unreadUpload) Read([]byte) (int, error) {
	reader.reads++
	return 0, errors.New("不应读取其他页面文件")
}

func TestSharedChromeUploadOwnershipAndAtMostOnce(t *testing.T) {
	options, _, sessions := testOptions()
	directory, err := os.MkdirTemp("/workspace", "browser-upload-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(directory, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(profile, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	service := launchTestBrowser(t, profile, options).service
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := fmt.Fprint(w, `<!doctype html><input id=files type=file multiple onchange="window.changes=(window.changes||0)+1;if(this.dataset.confirm)window.answer=confirm('确认选择文件')"><input id=text><input id=single type=file>`); err != nil {
			t.Error(err)
		}
	}))
	defer site.Close()
	address := Address{Session: sessions[0], WindowID: ids.New()}
	if _, err := service.Open(t.Context(), address); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(t.Context(), address, Command{Sequence: 1, Kind: "navigate", URL: site.URL}); err != nil {
		t.Fatal(err)
	}
	waitValue(t, service, address, `document.querySelector('#files')?'ready':'waiting'`, "ready")
	request := UploadRequest{Sequence: 2, Selector: "#files", Files: []UploadFile{{Name: "内容.txt", Content: []byte("原文")}}}
	send := func(request UploadRequest) (Window, error) {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return service.Upload(t.Context(), address, bytes.NewReader(encoded))
	}
	for range 2 {
		window, err := send(request)
		if err != nil || window.Sequence != 2 || window.CommandState != "completed" {
			t.Fatal("上传或重复回执失败", window.CommandState, err)
		}
	}
	if value := evaluate(t, service, address, `document.querySelector('#files').files[0].text().then(text=>text+'|'+window.changes)`); value != "原文|1" {
		t.Fatal("重复上传触发了网页动作", value)
	}
	request.Files[0].Content = []byte("替换")
	if _, err := send(request); !errors.Is(err, ErrConflict) {
		t.Fatal("相同序号替换内容被接受", err)
	}
	request.Sequence = 3
	request.Selector = "#text"
	if _, err := send(request); !errors.Is(err, ErrInvalid) {
		t.Fatal("非文件输入框被接受", err)
	}
	if value := evaluate(t, service, address, `String(window.changes)`); value != "1" {
		t.Fatal("拒绝操作产生副作用")
	}
	request.Selector = "#single"
	request.Files = append(request.Files, UploadFile{Name: "第二个.txt", Content: []byte("第二个文件")})
	if _, err := send(request); !errors.Is(err, ErrInvalid) {
		t.Fatal("单文件输入框接受多个文件", err)
	}
	request.Files = request.Files[:1]
	foreign := address
	foreign.Session = sessions[1]
	reader := &unreadUpload{}
	if _, err := service.Upload(t.Context(), foreign, reader); !errors.Is(err, ErrDenied) || reader.reads != 0 {
		t.Fatal("跨 Bot 上传读取了正文", reader.reads, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Upload(ctx, address, reader); err == nil || reader.reads != 0 {
		t.Fatal("取消后继续读取正文", err)
	}
	// 文件选择也会触发脚本；等待对话框期间必须允许顺序 Agent 答复，且不阻塞其他 Bot。
	if value := evaluate(t, service, address, `document.querySelector('#files').dataset.confirm='yes'`); value != "yes" {
		t.Fatal("无法设置文件选择确认框")
	}
	request.Selector = "#files"
	receipt, err := send(request)
	if err != nil || receipt.CommandState != "waiting_dialog" || receipt.Dialog.ID == 0 {
		t.Fatal("上传确认框没有返回等待回执", receipt.CommandState, err)
	}
	if duplicate, err := send(request); err != nil || duplicate != receipt {
		t.Fatal("等待中的上传没有复用回执", err)
	}
	other := Address{Session: sessions[1], WindowID: ids.New()}
	if _, err := service.Open(t.Context(), other); err != nil {
		t.Fatal("上传确认框阻塞其他 Bot 创建页面", err)
	}
	if _, err := service.Screenshot(t.Context(), other); err != nil {
		t.Fatal("上传确认框阻塞其他 Bot 截图", err)
	}
	if err := service.RespondDialog(t.Context(), address, DialogReply{ID: receipt.Dialog.ID, Accept: true}); err != nil {
		t.Fatal(err)
	}
	waitCommandState(t, service, address, request.Sequence, "completed")
	waitValue(t, service, address, `String(window.answer)+'|'+window.changes`, "true|2")
	if err := service.CloseWindow(t.Context(), address); err != nil {
		t.Fatal(err)
	}
	service.options.Uploads.mu.Lock()
	remaining := len(service.options.Uploads.batches)
	service.options.Uploads.mu.Unlock()
	if remaining != 0 {
		t.Fatal("页面关闭后未清理文件副本", remaining)
	}
}
