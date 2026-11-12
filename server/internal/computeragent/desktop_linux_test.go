//go:build linux

package computeragent

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func TestDesktopReadinessDeadlineDuringProbe(t *testing.T) {
	for _, insideProbe := range []bool{false, true} {
		t.Run(map[bool]string{false: "轮询间隙耗尽", true: "握手期间耗尽"}[insideProbe], func(t *testing.T) {
			failures := make(chan string, 2)
			desktop := &managedDesktop{status: computer.DesktopStatus{State: "starting"}, onFailure: func(code string) { failures <- code }}
			timeout := 20 * time.Millisecond
			if insideProbe {
				timeout = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			called := false
			desktop.awaitReady(ctx, func(ctx context.Context) (bool, error) { called = true; <-ctx.Done(); return false, ctx.Err() })
			if called != insideProbe {
				t.Fatal("未进入预期的总期限分支")
			}
			if desktop.snapshot().State != "failed" {
				t.Fatal("总期限耗尽后仍报告正在启动")
			}
			select {
			case code := <-failures:
				if code != "desktop_start_timeout" {
					t.Fatal(code)
				}
			default:
				t.Fatal("总期限耗尽未发出撤销")
			}
			select {
			case <-failures:
				t.Fatal("同一失败重复撤销")
			default:
			}
		})
	}
}

func TestDesktopReadinessCancellationDoesNotReportFailure(t *testing.T) {
	desktop := &managedDesktop{status: computer.DesktopStatus{State: "starting"}, onFailure: func(string) { t.Error("正常取消被报告为故障") }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	desktop.awaitReady(ctx, func(context.Context) (bool, error) { t.Error("取消后仍探测"); return true, nil })
}

// 图形会话退出独立于浏览器应用；退出码为零也必须撤销已丢失的桌面。
func TestDesktopExitFailsSessionAndReapsProcess(t *testing.T) {
	for _, exitCode := range []string{"0", "7"} {
		t.Run(exitCode, func(t *testing.T) {
			command := exec.Command("/bin/sh", "-c", "exit "+exitCode)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			failures := make(chan string, 2)
			desktop := &managedDesktop{status: computer.DesktopStatus{State: "starting"}, command: command,
				done: make(chan struct{}), probed: make(chan struct{}), stop: make(chan struct{}), onFailure: func(code string) { failures <- code }}
			desktop.supervise(func(context.Context) (bool, error) { return false, nil })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			select {
			case code := <-failures:
				if code != "desktop_exited" || desktop.snapshot().State != "failed" {
					t.Fatal("桌面退出没有撤销所属会话", code)
				}
			case <-ctx.Done():
				t.Fatal("图形会话退出未被监督器发现")
			}
			if err := desktop.closeAfterDrain(ctx); err != nil {
				t.Fatal(err)
			}
			if command.ProcessState == nil || !command.ProcessState.Exited() {
				t.Fatal("图形进程未完成回收")
			}
		})
	}
}
