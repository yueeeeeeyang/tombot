//go:build linux

package computeragent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/browserprobe"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

type browserProcess struct {
	mu                              sync.Mutex
	lifetime                        context.Context
	cancel                          context.CancelFunc
	reader, writer                  *os.File
	revoked, stopping               bool
	revokeError, failure, exitError error
	service                         *sharedbrowser.Service
	started, done                   chan struct{}
}

type browserCommandFactory func(taskprocess.BrowserRequest, *os.File, *os.File, *os.File, *os.File) (*exec.Cmd, io.Closer, error)

func newBrowserProcess() (*browserProcess, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.New("创建共享浏览器启动撤销管道失败")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &browserProcess{reader: reader, writer: writer, lifetime: lifetime, cancel: cancel, started: make(chan struct{}), done: make(chan struct{})}, nil
}

func prepareBrowserProcess(request taskprocess.BrowserRequest, membership, revocation, commands, responses *os.File) (*exec.Cmd, io.Closer, error) {
	sealed, err := taskprocess.SealBrowser(request)
	if err != nil {
		return nil, nil, err
	}
	command := exec.Command("/usr/lib/tombot/computer-task", "--browser")
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.ExtraFiles = []*os.File{sealed, membership, revocation, commands, responses}
	return command, sealed, nil
}

func (process *browserProcess) start(ctx context.Context, request taskprocess.BrowserRequest, membership *os.File, options sharedbrowser.Options, prepare browserCommandFactory) (service *sharedbrowser.Service, failure error) {
	launched := false
	defer func() {
		failure = errors.Join(failure, process.reader.Close())
		if !launched {
			close(process.done)
		}
		if failure != nil {
			failure = errors.Join(failure, process.revoke())
			process.cancel()
			service = nil
		}
		close(process.started)
	}()
	if ctx == nil || prepare == nil || options.Validate() != nil {
		return nil, errors.New("共享浏览器进程启动配置无效")
	}
	commands, send, err := os.Pipe()
	if err != nil {
		return nil, errors.New("创建共享浏览器命令管道失败")
	}
	responses, receive, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(errors.New("创建共享浏览器响应管道失败"), commands.Close(), send.Close())
	}
	defer func() {
		failure = errors.Join(failure, agentchannel.CloseFiles([]*os.File{commands, send, responses, receive}))
	}()
	command, sealed, err := prepare(request, membership, process.reader, commands, receive)
	if err != nil {
		return nil, err
	}
	if sealed != nil {
		defer func() { failure = errors.Join(failure, sealed.Close()) }()
	}
	if command == nil {
		return nil, errors.New("共享浏览器引导程序缺失")
	}
	// Chrome 诊断可能含网页地址或内容，只匹配有界前缀中的固定分类，不转发原始输出。
	diagnostics := &browserDiagnostics{}
	defer diagnostics.close()
	command.Stdout, command.Stderr = io.Discard, diagnostics
	command.WaitDelay = time.Second
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, browserprobe.StartFailure("browser_start_launch_failed", errors.New("启动共享浏览器引导失败"))
	}
	launched = true
	go func() {
		err := command.Wait()
		process.mu.Lock()
		if err != nil {
			process.exitError = errors.New("共享 Chrome 进程异常退出")
		}
		process.mu.Unlock()
		close(process.done)
	}()
	closeErr := errors.Join(commands.Close(), receive.Close())
	commands, receive = nil, nil
	if closeErr != nil {
		return nil, errors.New("关闭共享浏览器子进程管道副本失败")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	reader, writer := responses, send
	responses, send = nil, nil
	options.Uploads, err = sharedbrowser.NewUploadStore()
	if err != nil {
		return nil, errors.Join(browserprobe.StartFailure("browser_start_upload_failed", err), reader.Close(), writer.Close())
	}
	options.Native = sharedbrowser.NewNativeDesktop(request.RuntimeDirectory())
	service, err = sharedbrowser.New(process.lifetime, ctx, reader, writer, options)
	if err != nil {
		// CDP 管道可先于 stderr 排空关闭；有界等待 Wait 汇合，避免丢失紧邻退出的启动分类。
		drain, stop := context.WithTimeout(ctx, command.WaitDelay)
		select {
		case <-process.done:
		case <-drain.Done():
		}
		stop()
		code := diagnostics.failureCode()
		if code == "" {
			code = "browser_start_protocol_failed"
			select {
			case <-process.done:
				code = "browser_start_process_exited"
			default:
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = "browser_start_timeout"
			}
		}
		return nil, errors.Join(browserprobe.StartFailure(code, err), options.Uploads.Close())
	}
	process.mu.Lock()
	process.service = service
	process.mu.Unlock()
	select {
	case <-process.done:
		return nil, browserprobe.StartFailure("browser_start_process_exited", errors.New("共享 Chrome 在初始化期间退出"))
	default:
	}
	return service, ctx.Err()
}

func (process *browserProcess) recordFailure(err error) {
	if err == nil {
		return
	}
	process.mu.Lock()
	process.failure = errors.Join(process.failure, err)
	process.mu.Unlock()
}

func (process *browserProcess) revoke() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if !process.revoked {
		process.revoked = true
		if err := process.writer.Close(); err != nil {
			process.revokeError = errors.New("撤销共享浏览器迟到启动失败")
		}
	}
	return process.revokeError
}

func (process *browserProcess) isStopping() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.stopping
}

func (process *browserProcess) hasService() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.service != nil
}

func (process *browserProcess) close(ctx context.Context) error {
	process.mu.Lock()
	process.stopping = true
	process.mu.Unlock()
	failure := process.revoke()
	select {
	case <-process.started:
	default:
		process.cancel()
	}
	select {
	case <-process.started:
	case <-ctx.Done():
		return errors.Join(failure, ctx.Err())
	}
	process.mu.Lock()
	service := process.service
	process.mu.Unlock()
	if service != nil {
		failure = errors.Join(failure, service.Close(ctx))
	}
	process.cancel()
	return errors.Join(failure, process.wait(ctx))
}

func (process *browserProcess) wait(ctx context.Context) error {
	for _, completed := range []<-chan struct{}{process.started, process.done} {
		select {
		case <-completed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	// 启动失败由 Start 返回；未交付服务时，进程已退出即可与资源组一起完成回收核验。
	if process.service == nil {
		return errors.Join(process.failure, process.revokeError)
	}
	return errors.Join(process.exitError, process.failure, process.revokeError)
}

func (process *browserProcess) finished() bool {
	for _, completed := range []<-chan struct{}{process.started, process.done} {
		select {
		case <-completed:
		default:
			return false
		}
	}
	return true
}
