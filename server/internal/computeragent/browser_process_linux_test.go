//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

func browserFixture(t *testing.T) (taskprocess.BrowserRequest, sharedbrowser.Options, []computer.ExecutionSession) {
	t.Helper()
	fixture := newFixture(t)
	probe := probeGrant(fixture)
	probe.Request.Group.Kind = processgroup.BrowserService
	stamp, err := agentchannel.FromDeadline(time.Now().Add(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	request := taskprocess.BrowserRequest{Version: 1, Group: probe.Request.Group, Deadline: stamp}
	first := fixture.command.Session
	second := first
	second.BotID, second.DesktopID, second.SessionID, second.RunID = ids.New(), ids.New(), ids.New(), ids.New()
	second.UID, second.DisplayNumber = computer.DesktopUIDBase+2, 2
	options := sharedbrowser.Options{UserID: first.UserID, ComputerID: first.ComputerID, Generation: first.Generation,
		Width: 800, Height: 600, MaximumWindows: 4, MaximumPerSession: 2, MaximumWindowRecords: 16, MaximumEvents: 128,
		CallTimeout: time.Second, DialogTimeout: 10 * time.Second, ShutdownTimeout: time.Second, Logger: fixture.guard.logger,
		Authorize: func(_ context.Context, session computer.ExecutionSession) error {
			if session != first && session != second {
				return sharedbrowser.ErrDenied
			}
			return nil
		}}
	return request, options, []computer.ExecutionSession{first, second}
}

func TestBrowserProcessStartupFailures(t *testing.T) {
	for _, phase := range []string{"准备失败", "启动失败", "没有协议", "取消"} {
		t.Run(phase, func(t *testing.T) {
			request, options, _ := browserFixture(t)
			process, err := newBrowserProcess()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			prepare := func(taskprocess.BrowserRequest, *os.File, *os.File, *os.File, *os.File) (*exec.Cmd, io.Closer, error) {
				switch phase {
				case "准备失败":
					return nil, nil, errors.New("测试准备失败")
				case "启动失败":
					return exec.Command("/not-a-browser-program"), nil, nil
				case "取消":
					cancel()
				}
				return exec.Command("/bin/true"), nil, nil
			}
			service, err := process.start(ctx, request, nil, options, prepare)
			if err == nil || service != nil {
				t.Fatal("启动失败被报告为就绪")
			}
			cleanup, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := process.wait(cleanup); err == nil || !process.finished() {
				t.Fatal("启动失败未完整回收或丢失错误", err)
			}
			if _, err := process.writer.Write([]byte("x")); err == nil {
				t.Fatal("失败后启动授权仍然有效")
			}
		})
	}
}

func TestBrowserScopeRejectsWrongControlsAndRequest(t *testing.T) {
	request, options, _ := browserFixture(t)
	files := probeFiles(t)
	t.Cleanup(func() {
		if err := agentchannel.CloseFiles(files); err != nil {
			t.Error(err)
		}
	})
	factory := &kernelScopes{interval: time.Millisecond}
	if _, err := factory.OpenBrowser(request, files, time.Second); err == nil {
		t.Fatal("普通文件被用作浏览器组")
	}
	if _, err := factory.OpenBrowser(request, files, 0); err == nil {
		t.Fatal("缺少退出预算仍接管资源")
	}
	scope := &kernelBrowserScope{request: request, control: &kernelServiceScope{reference: request.Group, files: files}, done: make(chan struct{})}
	changed := request
	changed.Deadline.ExpiresNS++
	if _, err := scope.Start(t.Context(), changed, options); err == nil {
		t.Fatal("已打开服务的授权期限被覆盖")
	}
	scope.control.used = true
	if _, err := scope.Start(t.Context(), request, options); err == nil {
		t.Fatal("重复启动复用服务实例")
	}
	if err := scope.Close(); err == nil {
		t.Fatal("未清空资源组就关闭入口")
	}
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatal("拒绝接管损坏调用方入口", err)
		}
	}
}

func TestBrowserProtocolExitRevokesServiceAndNormalCloseDoesNot(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		t.Run(map[bool]string{false: "协议异常", true: "正常关闭"}[stopping], func(t *testing.T) {
			files := probeFiles(t)
			t.Cleanup(func() {
				if err := agentchannel.CloseFiles(files); err != nil {
					t.Error(err)
				}
			})
			process, err := newBrowserProcess()
			if err != nil {
				t.Fatal(err)
			}
			process.stopping = stopping
			close(process.started)
			scope := &kernelBrowserScope{control: &kernelServiceScope{files: files, interval: time.Millisecond}, process: process, done: make(chan struct{})}
			protocol := make(chan struct{})
			watched := make(chan struct{})
			go func() { scope.watch(process, protocol); close(watched) }()
			close(protocol)
			select {
			case <-watched:
			case <-time.After(time.Second):
				t.Fatal("协议结束没有通知监督者")
			}
			select {
			case <-scope.Done():
			default:
				t.Fatal("共享能力结束未通知")
			}
			metadata, err := files[1].Stat()
			if err != nil {
				t.Fatal(err)
			}
			if stopping && metadata.Size() != 0 || !stopping && metadata.Size() != 1 {
				t.Fatal("正常关闭和异常终止没有区分")
			}
			close(process.done)
			if err := process.reader.Close(); err != nil {
				t.Fatal(err)
			}
			err = process.close(t.Context())
			if stopping && err != nil || !stopping && err == nil {
				t.Fatal("共享浏览器异常被忽略", err)
			}
		})
	}
}

func TestBrowserScopeRevocationNotifiesBeforeStartup(t *testing.T) {
	request, options, _ := browserFixture(t)
	files := probeFiles(t)
	t.Cleanup(func() {
		if err := agentchannel.CloseFiles(files); err != nil {
			t.Error(err)
		}
	})
	scope := &kernelBrowserScope{request: request, control: &kernelServiceScope{reference: request.Group, files: files}, done: make(chan struct{})}
	if err := scope.Revoke(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scope.Done():
	default:
		t.Fatal("启动前撤销没有通知监督者")
	}
	if service, err := scope.Start(t.Context(), request, options); err == nil || service != nil {
		t.Fatal("撤销后仍允许迟到启动")
	}
}

func TestBrowserConcurrentRevocationAndClose(t *testing.T) {
	process, err := newBrowserProcess()
	if err != nil {
		t.Fatal(err)
	}
	close(process.started)
	close(process.done)
	if err := process.reader.Close(); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 32)
	for range 32 {
		wait.Go(func() { failures <- process.close(t.Context()) })
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal("并发关闭不幂等", err)
		}
	}
	if !process.finished() {
		t.Fatal("正常进程被误判未结束")
	}
}
