//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

const desktopStartTimeout = 45 * time.Second

type managedDesktop struct {
	mu           sync.Mutex
	status       computer.DesktopStatus
	command      *exec.Cmd
	stopping     bool
	stopOnce     sync.Once
	stop         chan struct{}
	done, probed chan struct{}
	onFailure    func(string)
	access       *desktopAccess
}

// StartDesktop 只启动该授权唯一的图形会话；重发查询同一实例，不重启未知或失败的桌面。
func (scope *kernelScope) StartDesktop(ctx context.Context, reference processgroup.Reference, session computer.ExecutionSession, membership, revocation *os.File, onFailure func(string)) (computer.DesktopStatus, error) {
	if session.Validate() != nil || reference.Validate() != nil || reference.Session != session || onFailure == nil || membership == nil || revocation == nil {
		return computer.DesktopStatus{}, errors.New("图形会话启动授权无效")
	}
	scope.mu.Lock()
	if scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session {
		scope.mu.Unlock()
		return computer.DesktopStatus{}, errors.New("图形会话桌面已关闭或身份不符")
	}
	if scope.desktop != nil {
		desktop := scope.desktop
		scope.mu.Unlock()
		return desktop.snapshot(), nil
	}
	desktop := &managedDesktop{status: computer.DesktopStatus{Session: session, State: "starting"}, done: make(chan struct{}), probed: make(chan struct{}), stop: make(chan struct{}), onFailure: onFailure}
	desktop.access = newDesktopAccess(session, func(ctx context.Context) (net.Conn, error) {
		return openDesktopConnection(ctx, session, "/"+reference.Name()+"/shell")
	})
	scope.desktop = desktop
	directory := scope.directory
	scope.mu.Unlock()
	request := taskprocess.Request{Version: 2, OperationID: session.SessionID, Group: reference, Role: "shell", ProcessCommand: computer.ProcessCommand{Program: "/usr/bin/python3", WorkingDirectory: "/workspace", Arguments: []string{"-I", "-B", "/usr/lib/tombot/desktop.py", "--runtime", session.RuntimeDirectory(), "--display", strconv.Itoa(session.DisplayNumber)}}}
	command, err := startDesktopTask(ctx, request, membership, revocation)
	desktop.command = command
	if command != nil && command.Process != nil {
		desktop.supervise(func(ctx context.Context) (bool, error) {
			ready, err := directory.readyFile()
			if err != nil || !ready {
				return ready, err
			}
			err = probeDesktopSocket(ctx, session, "/"+reference.Name()+"/shell")
			return err == nil, err
		})
	} else {
		close(desktop.done)
		close(desktop.probed)
	}
	if err != nil {
		desktop.fail("desktop_start_failed")
	}
	return desktop.snapshot(), nil
}

// startDesktopTask 使用同一封存协议启动固定镜像程序，部分启动也必须纳入后续 Wait 和资源组回收。
func startDesktopTask(ctx context.Context, request taskprocess.Request, membership, revocation *os.File) (*exec.Cmd, error) {
	command, sealed, err := prepareTask(request, membership, revocation)
	if err == nil {
		// 引导只输出固定状态与错误，用户桌面的各组件日志保留在私有运行目录。
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err = ctx.Err(); err == nil {
			err = command.Start()
		}
		if sealed.Close() != nil {
			err = errors.Join(err, errors.New("关闭图形会话封存请求失败"))
		}
	}
	return command, err
}

func (scope *kernelScope) InspectDesktop(_ context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if session.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session {
		return computer.DesktopStatus{}, errors.New("图形会话查询授权无效")
	}
	if scope.desktop == nil {
		return computer.DesktopStatus{Session: session, State: "absent"}, nil
	}
	return scope.desktop.snapshot(), nil
}

func (desktop *managedDesktop) snapshot() computer.DesktopStatus {
	desktop.mu.Lock()
	defer desktop.mu.Unlock()
	return desktop.status
}

func (desktop *managedDesktop) fail(code string) {
	desktop.mu.Lock()
	if desktop.stopping || desktop.status.State == "failed" {
		desktop.mu.Unlock()
		return
	}
	desktop.status.State, desktop.status.ErrorCode = "failed", code
	desktop.status.Width, desktop.status.Height = 0, 0
	desktop.mu.Unlock()
	desktop.onFailure(code)
}

func (desktop *managedDesktop) supervise(probe func(context.Context) (bool, error)) {
	go func() {
		defer close(desktop.done)
		err := desktop.command.Wait()
		// XFCE 会话即使以 0 退出也已不再提供桌面；正常回收会先设置 stopping。
		if err != nil || desktop.command.ProcessState != nil {
			desktop.fail("desktop_exited")
		}
	}()
	go func() {
		defer close(desktop.probed)
		ctx, cancel := context.WithTimeout(context.Background(), desktopStartTimeout)
		defer cancel()
		stopped := make(chan struct{})
		go func() {
			select {
			case <-desktop.stop:
				cancel()
			case <-desktop.done:
				cancel()
			case <-ctx.Done():
			}
			close(stopped)
		}()
		defer func() { cancel(); <-stopped }()
		desktop.awaitReady(ctx, probe)
	}()
}

// awaitReady 同时覆盖轮询间隙与握手内部耗尽总期限，任何超时都必须撤销启动授权。
func (desktop *managedDesktop) awaitReady(ctx context.Context, probe func(context.Context) (bool, error)) {
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			desktop.fail("desktop_start_timeout")
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		call, finish := context.WithTimeout(ctx, 2*time.Second)
		ready, err := probe(call)
		finish()
		if err != nil {
			if ctx.Err() == nil {
				desktop.fail("desktop_readiness_failed")
			}
			return
		}
		if ready && ctx.Err() == nil {
			desktop.mu.Lock()
			if !desktop.stopping && desktop.status.State == "starting" {
				desktop.status.State = "ready"
				desktop.status.Width = computer.DesktopWidth
				desktop.status.Height = computer.DesktopHeight
			}
			desktop.mu.Unlock()
			return
		}
	}
}

func (desktop *managedDesktop) stoppingNow() {
	if desktop.access != nil {
		desktop.access.stop()
	}
	desktop.mu.Lock()
	desktop.stopping = true
	desktop.mu.Unlock()
	desktop.stopOnce.Do(func() { close(desktop.stop) })
}

func (desktop *managedDesktop) closeAfterDrain(ctx context.Context) error {
	desktop.stoppingNow()
	if desktop.access != nil {
		if err := desktop.access.close(ctx); err != nil {
			return err
		}
	}
	for _, done := range []<-chan struct{}{desktop.done, desktop.probed} {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("图形会话引导或就绪检查回收未完成")
		}
	}
	return nil
}
