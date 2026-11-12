//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type kernelScopes struct {
	interval time.Duration
	windows  WindowFactory
}
type kernelScope struct {
	mu              sync.Mutex
	files           []*os.File
	interval        time.Duration
	drained, closed bool
	revoked         bool
	closeError      error
	directory       *desktopDirectory
	membershipUsed  bool
	terminals       map[string]*terminalAttempt
	desktop         *managedDesktop
	agent           *agentAttempt
	windows         WindowSession
}

// NewKernelScopes 要求受信 Linux root 代理和完整的窗口工厂；具体资源身份在每次接收描述符时复验。
func NewKernelScopes(interval time.Duration, windows WindowFactory) (ScopeFactory, error) {
	if os.Geteuid() != 0 || interval < time.Millisecond || interval > 100*time.Millisecond || windows == nil {
		return nil, errors.New("电脑代理资源组后端配置无效")
	}
	return &kernelScopes{interval: interval, windows: windows}, nil
}

func (factory *kernelScopes) Open(reference processgroup.Reference, files []*os.File) (Scope, error) {
	if factory.windows == nil {
		return nil, errors.New("电脑代理没有配置会话窗口入口")
	}
	if err := processgroup.ValidateControls(reference, files); err != nil {
		return nil, err
	}
	scope := &kernelScope{files: files, interval: factory.interval}
	busy, err := scope.populated()
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, errors.New("电脑代理不能接管已有未知进程的资源组")
	}
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, reference.Session)
	if err != nil {
		return nil, err
	}
	scope.directory = directory
	scope.windows, err = factory.windows(reference)
	if err != nil || scope.windows == nil {
		if scope.windows != nil {
			scope.windows.Revoke()
			ctx, cancel := context.WithTimeout(context.Background(), terminalCloseTimeout)
			closeError := scope.windows.Close(ctx)
			cancel()
			if closeError != nil {
				return nil, errors.Join(err, closeError)
			}
		}
		return nil, errors.Join(errors.New("电脑代理会话窗口入口未完成接管"), err, directory.Close())
	}
	return scope, nil
}

func (scope *kernelScope) Membership(role string) (*os.File, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed || scope.drained || scope.revoked {
		return nil, errors.New("电脑代理资源组已关闭")
	}
	index := 0
	if role == "browser" {
		index = 1
	} else if role != "shell" {
		return nil, errors.New("电脑代理进程用途无效")
	}
	fd, err := unix.FcntlInt(scope.files[index].Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, errors.New("复制电脑代理任务入组描述符失败")
	}
	scope.membershipUsed = true
	return os.NewFile(uintptr(fd), "task-membership"), nil
}

// Revoke 独立发送整组终止信号，不等待业务状态刷盘；成功仍不能代替存活状态确认。
func (scope *kernelScope) Revoke() error {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.drained {
		return nil
	}
	if scope.closed {
		return errors.New("电脑代理资源组控制入口提前关闭")
	}
	scope.revoked = true
	if scope.desktop != nil {
		scope.desktop.stoppingNow()
	}
	if scope.windows != nil {
		scope.windows.Revoke()
	}
	var failure error
	if scope.agent != nil && scope.agent.agent != nil {
		if err := scope.agent.agent.closeStreams(); err != nil {
			failure = errors.New("关闭 Bot Agent 私有流失败")
		}
	}
	if count, err := scope.files[2].WriteAt([]byte("1"), 0); err != nil || count != 1 {
		return errors.Join(failure, errors.New("电脑代理终止整个会话资源组失败"))
	}
	return failure
}

func (scope *kernelScope) populated() (bool, error) {
	encoded := make([]byte, 8193)
	count, err := scope.files[3].ReadAt(encoded, 0)
	if err != nil && !errors.Is(err, io.EOF) || count < 1 || count == len(encoded) {
		return false, errors.New("读取电脑代理资源组存活状态失败")
	}
	return processgroup.Populated(encoded[:count])
}

func (scope *kernelScope) Terminate(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scope.Revoke(); err != nil {
		return err
	}
	ticker := time.NewTicker(scope.interval)
	defer ticker.Stop()
	for {
		scope.mu.Lock()
		if scope.drained {
			scope.mu.Unlock()
			return nil
		}
		if scope.closed {
			scope.mu.Unlock()
			return errors.New("电脑代理资源组控制入口提前关闭")
		}
		busy, err := scope.populated()
		if err != nil {
			scope.mu.Unlock()
			return err
		}
		if !busy {
			scope.mu.Unlock()
			if scope.windows != nil {
				if err := scope.windows.Close(ctx); err != nil {
					return err
				}
			}
			scope.mu.Lock()
			scope.drained = true
			scope.mu.Unlock()
			return nil
		}
		scope.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (scope *kernelScope) Close() error {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.membershipUsed && !scope.drained {
		return errors.New("电脑任务未确认回收，不能清理桌面身份目录")
	}
	if !scope.closed {
		ctx, cancel := context.WithTimeout(context.Background(), terminalCloseTimeout)
		defer cancel()
		if scope.windows != nil {
			if err := scope.windows.Close(ctx); err != nil {
				return err
			}
		}
		if scope.agent != nil && scope.agent.agent != nil {
			if err := scope.agent.agent.closeAfterDrain(ctx); err != nil {
				return err
			}
		}
		if scope.desktop != nil {
			if err := scope.desktop.closeAfterDrain(ctx); err != nil {
				return err
			}
		}
		for _, attempt := range scope.terminals {
			if attempt.terminal != nil {
				if err := attempt.terminal.closeAfterDrain(ctx); err != nil {
					return err
				}
			}
		}
		if scope.directory != nil {
			if err := scope.directory.Close(); err != nil {
				return err
			}
		}
		scope.closed = true
		scope.closeError = agentchannel.CloseFiles(scope.files)
	}
	return scope.closeError
}
