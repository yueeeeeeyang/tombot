//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

func TestKernelConsoleFreezesTasksBeforeUserAccess(t *testing.T) {
	f := newFixture(t)
	session := f.command.Session
	scope := &kernelScope{interval: time.Millisecond, directory: &desktopDirectory{session: session}, desktop: &managedDesktop{status: computer.DesktopStatus{Session: session, State: "ready"}}}
	for range processgroup.ExecutionControlFiles {
		file, err := os.CreateTemp(t.TempDir(), "group")
		if err != nil {
			t.Fatal(err)
		}
		scope.files = append(scope.files, file)
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := scope.files[5].WriteAt([]byte("populated 1\nfrozen 1\n"), 0); err != nil {
		t.Fatal(err)
	}
	request := computer.ConsoleRequest{Session: session, ViewerID: session.SessionID, Revision: 1, Action: "capture"}
	if _, err := scope.Console(t.Context(), request); err == nil {
		t.Fatal("未暂停任务就允许接管")
	}
	request.Action = "pause"
	receipt, err := scope.Console(t.Context(), request)
	if err != nil || receipt.Validate(request) != nil || !scope.isPaused {
		t.Fatal("暂停没有确认", receipt, err)
	}
	state := make([]byte, 1)
	if _, err := scope.files[4].ReadAt(state, 0); err != nil || !bytes.Equal(state, []byte("1")) {
		t.Fatal("未写入冻结状态", err)
	}
	if _, err := scope.Console(t.Context(), request); err != nil {
		t.Fatal("控制重试不幂等", err)
	}
	if _, err := scope.desktopAccess(session); err == nil {
		t.Fatal("接管期间 Agent 仍可输入")
	}

	request.Action = "resume"
	if _, err := scope.Console(t.Context(), request); err == nil {
		t.Fatal("同代次不同控制被接受")
	}
	request.Revision++
	if _, err := scope.files[5].WriteAt([]byte("populated 1\nfrozen 0\n"), 0); err != nil {
		t.Fatal(err)
	}
	receipt, err = scope.Console(t.Context(), request)
	if err != nil || receipt.Validate(request) != nil || scope.isPaused || scope.consoleViewer != "" {
		t.Fatal("恢复状态错误", err)
	}
	request.Revision, request.Action = 1, "pause"
	if _, err := scope.Console(t.Context(), request); err == nil {
		t.Fatal("旧接管窗口仍可读取屏幕")
	}
}

func TestKernelFreezeMustBeConfirmed(t *testing.T) {
	for _, encoded := range []string{"", "populated 1", "frozen 2", "frozen 1\nfrozen 0", "frozen"} {
		if _, err := frozenState([]byte(encoded)); err == nil {
			t.Fatal("无效冻结回执被接受", encoded)
		}
	}
	freeze, err := os.CreateTemp(t.TempDir(), "freeze")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := freeze.Close(); err != nil {
			t.Error(err)
		}
	}()
	events, err := os.CreateTemp(t.TempDir(), "events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := events.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := events.WriteString("populated 1\nfrozen 0\n"); err != nil {
		t.Fatal(err)
	}
	scope := &kernelScope{interval: time.Millisecond, files: []*os.File{nil, nil, nil, nil, freeze, events}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := scope.freezeTasks(ctx, true); err == nil {
		t.Fatal("未冻结却返回成功")
	}
}
