//go:build linux

package computeragent

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/sharedbrowser"
)

type trackedWindows struct {
	revoked atomic.Bool
	closed  atomic.Bool
	close   func(context.Context) error
}

func (windows *trackedWindows) Revoke() { windows.revoked.Store(true) }
func (windows *trackedWindows) UseBrowser(context.Context, computer.ExecutionSession, func(context.Context, sharedbrowser.Browser) error) error {
	return sharedbrowser.ErrUnavailable
}
func (windows *trackedWindows) Close(ctx context.Context) error {
	if !windows.revoked.Load() {
		return errors.New("窗口未撤销就回收页面")
	}
	if windows.closed.Load() {
		return nil
	}
	if err := windows.close(ctx); err != nil {
		return err
	}
	windows.closed.Store(true)
	return nil
}

// TestKernelScopeWindowRetirement 使用文件替身模拟组状态，仅验证窗口回收与会话状态的组合顺序。
func TestKernelScopeWindowRetirement(t *testing.T) {
	for _, fails := range []bool{false, true} {
		scope := &kernelScope{interval: time.Millisecond, membershipUsed: true}
		for index := range 4 {
			file, err := os.CreateTemp(t.TempDir(), "group")
			if err != nil {
				t.Fatal(err)
			}
			scope.files = append(scope.files, file)
			t.Cleanup(func() {
				if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Error(err)
				}
			})
			if index == 3 {
				if _, err := file.WriteString("populated 0\n"); err != nil {
					t.Fatal(err)
				}
			}
		}
		fault := errors.New("所属页面未回收")
		windows := &trackedWindows{close: func(ctx context.Context) error {
			if !scope.mu.TryLock() {
				return errors.New("回收页面时持有会话资源锁")
			}
			if scope.drained {
				t.Error("回收页面前提前确认会话已排空")
			}
			scope.mu.Unlock()
			if fails {
				return fault
			}
			return ctx.Err()
		}}
		scope.windows = windows
		err := scope.Terminate(t.Context())
		if fails {
			if !errors.Is(err, fault) || scope.drained || scope.Close() == nil {
				t.Fatal("窗口失败被整组空闲掩盖", err)
			}
		} else {
			if err != nil || !scope.drained || !windows.closed.Load() {
				t.Fatal("会话未回收所属窗口", err)
			}
			if err := scope.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
