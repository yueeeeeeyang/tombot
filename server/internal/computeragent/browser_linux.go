//go:build linux

package computeragent

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

type kernelBrowserScope struct {
	control   *kernelServiceScope
	request   taskprocess.BrowserRequest
	directory *browserDirectory
	grace     time.Duration
	process   *browserProcess
	done      chan struct{}
	doneOnce  sync.Once
	closeMu   sync.Mutex
}

func (factory *kernelScopes) OpenBrowser(request taskprocess.BrowserRequest, files []*os.File, grace time.Duration) (BrowserScope, error) {
	if request.Validate() != nil || grace < time.Second || grace > 30*time.Second {
		return nil, errors.New("共享浏览器服务配置无效")
	}
	if _, err := request.Deadline.Local(taskprocess.BrowserMaximumStartTime); err != nil {
		return nil, err
	}
	control, err := factory.openService(request.Group, files)
	if err != nil {
		return nil, err
	}
	directory, err := newBrowserDirectory(request)
	if err != nil {
		return nil, err
	}
	return &kernelBrowserScope{control: control, request: request, directory: directory, grace: grace, done: make(chan struct{})}, nil
}

func (scope *kernelBrowserScope) Start(ctx context.Context, request taskprocess.BrowserRequest, options sharedbrowser.Options) (BrowserService, error) {
	if ctx == nil || request.Validate() != nil || request != scope.request || request.Group != scope.control.reference || request.Group.Kind != processgroup.BrowserService || options.Validate() != nil {
		return nil, errors.New("共享浏览器启动参数或服务归属无效")
	}
	deadline, err := request.Deadline.Local(taskprocess.BrowserMaximumStartTime)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return nil, err
	}
	scope.control.mu.Lock()
	if scope.control.used || scope.control.revoked || scope.control.closed {
		scope.control.mu.Unlock()
		return nil, errors.New("共享浏览器服务实例已经使用或关闭")
	}
	process, err := newBrowserProcess()
	if err != nil {
		scope.control.mu.Unlock()
		return nil, err
	}
	scope.control.used, scope.process = true, process
	scope.control.mu.Unlock()
	stop := context.AfterFunc(bounded, func() { process.recordFailure(scope.Revoke()) })
	service, err := process.start(bounded, request, scope.control.files[0], options, prepareBrowserProcess)
	stop()
	if err != nil || bounded.Err() != nil {
		scope.doneOnce.Do(func() { close(scope.done) })
		return nil, errors.Join(err, bounded.Err(), scope.Revoke())
	}
	scope.control.mu.Lock()
	revoked := scope.control.revoked || scope.control.closed
	scope.control.mu.Unlock()
	if revoked {
		return nil, errors.Join(errors.New("共享浏览器启动期间被撤销"), scope.Revoke())
	}
	go scope.watch(process, service.Done())
	return service, nil
}

func (scope *kernelBrowserScope) watch(process *browserProcess, serviceDone <-chan struct{}) {
	select {
	case <-process.done:
	case <-serviceDone:
	}
	scope.doneOnce.Do(func() { close(scope.done) })
	if !process.isStopping() {
		process.recordFailure(errors.New("共享浏览器进程或协议异常结束"))
		process.recordFailure(scope.Revoke())
	}
}

func (scope *kernelBrowserScope) Done() <-chan struct{} { return scope.done }

// Revoke 先封闭迟到启动，再终止基础服务组；不等待某个 Bot 的窗口或桌面锁。
func (scope *kernelBrowserScope) Revoke() error {
	scope.control.mu.Lock()
	scope.control.revoked = true
	process := scope.process
	scope.control.mu.Unlock()
	scope.doneOnce.Do(func() { close(scope.done) })
	var failure error
	if process != nil {
		failure = process.revoke()
		process.cancel()
	}
	return errors.Join(failure, scope.control.Revoke())
}

// Drain 给 Chrome 有界正常退出时间以保存 profile，随后仍核验并回收整组后代。
// 强制终止、退出异常或超时明确失败，不能被解释为账号状态已干净保存。
func (scope *kernelBrowserScope) Drain(ctx context.Context) error {
	if ctx == nil {
		return errors.New("共享浏览器回收上下文缺失")
	}
	scope.control.mu.Lock()
	scope.control.revoked = true
	process := scope.process
	scope.control.mu.Unlock()
	var graceful error
	if process != nil {
		bounded, cancel := context.WithTimeout(ctx, scope.grace)
		graceful = process.close(bounded)
		cancel()
	}
	controlErr := scope.control.Drain(ctx)
	var waitErr error
	if process != nil {
		waitErr = process.wait(ctx)
	}
	scope.doneOnce.Do(func() { close(scope.done) })
	return errors.Join(graceful, controlErr, waitErr)
}

func (scope *kernelBrowserScope) Close() error {
	scope.closeMu.Lock()
	defer scope.closeMu.Unlock()
	scope.control.mu.Lock()
	process, drained := scope.process, scope.control.drained
	scope.control.mu.Unlock()
	if !drained || process != nil && !process.finished() {
		return errors.New("共享浏览器启动或整组进程尚未确认结束")
	}
	return errors.Join(scope.control.Close(), scope.directory.Close())
}

var _ BrowserFactory = (*kernelScopes)(nil)
