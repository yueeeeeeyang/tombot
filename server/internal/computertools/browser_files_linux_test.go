//go:build linux

package computertools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
)

func TestBrowserUploadReadsOnlyBoundedRegularFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "中文.txt")
	if err := os.WriteFile(path, []byte("共享资料"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := readBrowserFiles(t.Context(), []string{path})
	if err != nil || len(files) != 1 || files[0].Name != "中文.txt" || string(files[0].Content) != "共享资料" {
		t.Fatal(files, err)
	}
	link := filepath.Join(directory, "link.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBrowserFiles(t.Context(), []string{link}); err != nil {
		t.Fatal("共享文件正常链接被拒绝", err)
	}
	pipe := filepath.Join(directory, "pipe")
	if err := unix.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{directory, pipe, "/dev/zero", filepath.Join(directory, "missing")} {
		if _, err := readBrowserFiles(t.Context(), []string{invalid}); !errors.Is(err, sharedbrowser.ErrInvalid) {
			t.Fatal("特殊或缺失文件被接受", invalid, err)
		}
	}
	if err := os.Truncate(path, sharedbrowser.MaximumUploadBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readBrowserFiles(t.Context(), []string{path}); !errors.Is(err, sharedbrowser.ErrCapacity) {
		t.Fatal("文件上限未生效", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readBrowserFiles(ctx, []string{path}); !errors.Is(err, context.Canceled) {
		t.Fatal("取消后继续读取", err)
	}
}

func TestBrowserUploadKeepsKnownAndUnknownFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{sharedbrowser.ErrInvalid, sharedbrowser.ErrDialog, sharedbrowser.ErrCapacity, sharedbrowser.ErrUnknown, sharedbrowser.ErrUnavailable} {
		session := shellAddress().Session
		id := ids.New()
		stub := &localBrowserStub{state: sharedbrowser.State{Version: 2, Width: 960, Height: 640, Windows: []sharedbrowser.Window{{ID: id, BotID: session.BotID, DesktopID: session.DesktopID, State: "ready"}}}, uploadError: failure}
		tool := &Browser{connect: func(computer.ExecutionSession) (browserClient, error) { return stub, nil }}
		arguments, err := json.Marshal(map[string]any{"action": "upload", "window_id": id, "selector": "#files", "paths": []string{path}})
		if err != nil {
			t.Fatal(err)
		}
		grant := botagent.ToolGrant{OperationID: ids.New(), Sequence: 1, Session: session, ApprovalMode: "standard", TimeoutMS: 1000, OutputBytes: 4096}
		result, err := tool.Execute(t.Context(), grant, botagent.ToolCall{Name: "computer_browser", Arguments: string(arguments)})
		unknown := errors.Is(failure, sharedbrowser.ErrUnknown) || errors.Is(failure, sharedbrowser.ErrUnavailable)
		if unknown && err == nil || !unknown && (err != nil || !result.IsError) || stub.actions != 1 {
			t.Fatal("上传失败被成功结果掩盖", failure, result, err)
		}
	}
}
