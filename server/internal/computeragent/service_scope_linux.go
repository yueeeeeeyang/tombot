//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/processgroup"
)

type kernelServiceScope struct {
	mu                             sync.Mutex
	reference                      processgroup.ServiceReference
	files                          []*os.File
	interval                       time.Duration
	used, revoked, drained, closed bool
	closeError                     error
}

// openService 统一核验容器基础服务的三个内核入口，成功后接管描述符。
func (factory *kernelScopes) openService(reference processgroup.ServiceReference, files []*os.File) (*kernelServiceScope, error) {
	if err := processgroup.ValidateServiceControls(reference, files); err != nil {
		return nil, err
	}
	scope := &kernelServiceScope{reference: reference, files: files, interval: factory.interval}
	busy, err := scope.populated()
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, errors.New("电脑代理不能接管已有进程的基础服务组")
	}
	return scope, nil
}

// Revoke 与 Run 独立，不等待命令执行或持久记录；空组仍须发出关闭信号封闭后续使用。
func (scope *kernelServiceScope) Revoke() error {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.drained {
		return nil
	}
	if scope.closed {
		return errors.New("基础服务控制入口提前关闭")
	}
	scope.revoked = true
	if count, err := scope.files[1].WriteAt([]byte("1"), 0); err != nil || count != 1 {
		return errors.New("终止基础服务组失败")
	}
	return nil
}

func (scope *kernelServiceScope) populated() (bool, error) {
	encoded := make([]byte, 8193)
	count, err := scope.files[2].ReadAt(encoded, 0)
	if err != nil && !errors.Is(err, io.EOF) || count < 1 || count == len(encoded) {
		return false, errors.New("读取基础服务组存活状态失败")
	}
	return processgroup.Populated(encoded[:count])
}

func (scope *kernelServiceScope) Drain(ctx context.Context) error {
	if err := scope.Revoke(); err != nil {
		return err
	}
	ticker := time.NewTicker(scope.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		scope.mu.Lock()
		if scope.closed {
			scope.mu.Unlock()
			return errors.New("基础服务控制入口提前关闭")
		}
		busy, err := scope.populated()
		if err == nil && !busy {
			scope.drained = true
		}
		scope.mu.Unlock()
		if err != nil {
			return err
		}
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (scope *kernelServiceScope) Close() error {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if !scope.drained {
		return errors.New("基础服务组尚未确认回收")
	}
	if !scope.closed {
		scope.closed = true
		scope.closeError = agentchannel.CloseFiles(scope.files)
	}
	return scope.closeError
}
