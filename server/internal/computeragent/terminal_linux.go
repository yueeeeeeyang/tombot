//go:build linux

package computeragent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
	"io.yueyang/tom/internal/terminalio"
)

const (
	maximumTerminals     = 16
	terminalBufferBytes  = 256 << 10
	terminalWriteTimeout = 2 * time.Second
	terminalCloseTimeout = 2 * time.Second
)

type terminalAttempt struct {
	digest   [32]byte
	terminal *managedTerminal
	failure  error
}

// managedTerminal 的生存期属于桌面资源组，HTTP 断线、单个 Shell 退出均不撤销其他终端。
type managedTerminal struct {
	master         *os.File
	command        *exec.Cmd
	mu             sync.Mutex
	output         terminalio.Buffer
	exited         bool
	exitCode       int
	errorCode      string
	done, readDone chan struct{}
	input          sync.Mutex
	lastInput      [32]byte
	receipt        computer.TerminalReceipt
	closeOnce      sync.Once
	closeError     error
}

// StartTerminal 只执行固定引导程序；完整请求通过封存描述符传递，用户工作目录在降权之后设置。
func (scope *kernelScope) StartTerminal(ctx context.Context, reference processgroup.Reference, request computer.TerminalStart, membership, revocation *os.File) (computer.TerminalOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalOutput{}, err
	}
	if reference.Validate() != nil || reference.Session != request.Session {
		return computer.TerminalOutput{}, errors.New("终端资源组身份不符")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return computer.TerminalOutput{}, errors.New("终端启动内容编码失败")
	}
	digest := sha256.Sum256(encoded)
	scope.mu.Lock()
	if scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != request.Session {
		scope.mu.Unlock()
		return computer.TerminalOutput{}, errors.New("终端桌面已关闭或身份不符")
	}
	if saved, exists := scope.terminals[request.OperationID]; exists {
		attempt := *saved
		scope.mu.Unlock()
		if attempt.digest != digest {
			return computer.TerminalOutput{}, errors.New("终端启动标识已被其他内容使用")
		}
		if attempt.failure != nil {
			return computer.TerminalOutput{}, attempt.failure
		}
		if attempt.terminal == nil {
			return computer.TerminalOutput{}, errors.New("终端启动结果尚未确认")
		}
		return attempt.terminal.read(0, computer.MaximumTerminalReadBytes)
	}
	if len(scope.terminals) >= maximumTerminals {
		scope.mu.Unlock()
		return computer.TerminalOutput{}, errors.New("当前执行会话的终端启动数量已达上限")
	}
	if scope.terminals == nil {
		scope.terminals = make(map[string]*terminalAttempt)
	}
	attempt := &terminalAttempt{digest: digest}
	scope.terminals[request.OperationID] = attempt
	scope.mu.Unlock()
	// 桌面守卫串行化此方法；独立撤销仅取得短锁，不能等待本地进程启动。
	terminal, err := startManagedTerminal(ctx, reference, request, membership, revocation)
	scope.mu.Lock()
	attempt.terminal, attempt.failure = terminal, err
	scope.mu.Unlock()
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	return terminal.read(0, computer.MaximumTerminalReadBytes)
}

func startManagedTerminal(ctx context.Context, reference processgroup.Reference, request computer.TerminalStart, membership, revocation *os.File) (_ *managedTerminal, failure error) {
	command, sealed, err := prepareTask(taskprocess.Request{Version: 2, OperationID: request.OperationID, Group: reference, Role: "shell", ProcessCommand: request.Command}, membership, revocation)
	if err != nil {
		return nil, err
	}
	defer func() {
		if sealed.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭终端封存请求失败"))
		}
	}()
	master, slave, err := pty.Open()
	if err != nil {
		return nil, errors.New("分配容器终端失败")
	}
	defer func() {
		if slave.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭终端从端失败"))
		}
	}()
	if err = slave.Chown(request.Session.UID, request.Session.GID); err == nil {
		err = slave.Chmod(0600)
	}
	if err == nil {
		err = terminalio.ResizePTY(master, request.Columns, request.Rows)
	}
	if err != nil {
		return nil, errors.Join(errors.New("绑定终端设备身份或尺寸失败"), master.Close())
	}
	master, err = terminalio.PollablePTY(master)
	if err != nil {
		return nil, errors.New("准备终端有界读写失败")
	}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = ctx.Err(); err == nil {
		err = command.Start()
	}
	if err != nil {
		return nil, errors.Join(errors.New("启动受信终端引导失败"), master.Close())
	}
	terminal := &managedTerminal{master: master, command: command, output: terminalio.NewBuffer(terminalBufferBytes), exitCode: -1, done: make(chan struct{}), readDone: make(chan struct{})}
	go terminal.readOutput()
	go terminal.wait()
	return terminal, nil
}

func (scope *kernelScope) terminal(address computer.TerminalAddress) (*managedTerminal, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if address.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != address.Session {
		return nil, errors.New("终端桌面授权无效")
	}
	attempt := scope.terminals[address.OperationID]
	if attempt == nil || attempt.terminal == nil {
		return nil, errors.New("终端不存在或启动尚未确认")
	}
	return attempt.terminal, nil
}

func (scope *kernelScope) ReadTerminal(_ context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalOutput{}, err
	}
	terminal, err := scope.terminal(request.TerminalAddress)
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	return terminal.read(request.Cursor, request.MaximumBytes)
}

func (scope *kernelScope) WriteTerminal(ctx context.Context, request computer.TerminalWrite) (computer.TerminalReceipt, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalReceipt{}, err
	}
	terminal, err := scope.terminal(request.TerminalAddress)
	if err != nil {
		return computer.TerminalReceipt{}, err
	}
	return terminal.write(ctx, request.Sequence, request.Text)
}

func (scope *kernelScope) ResizeTerminal(ctx context.Context, request computer.TerminalResize) error {
	if err := request.Validate(); err != nil {
		return err
	}
	terminal, err := scope.terminal(request.TerminalAddress)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	terminal.mu.Lock()
	defer terminal.mu.Unlock()
	if terminal.exited {
		return errors.New("终端程序已退出")
	}
	if terminalio.ResizePTY(terminal.master, request.Columns, request.Rows) != nil {
		return errors.New("调整终端尺寸失败")
	}
	return nil
}

func (terminal *managedTerminal) read(cursor int64, limit int) (computer.TerminalOutput, error) {
	terminal.mu.Lock()
	defer terminal.mu.Unlock()
	if cursor > terminal.output.End {
		return computer.TerminalOutput{}, errors.New("终端游标超过已产生的输出")
	}
	text, next, truncated := terminal.output.Read(cursor, min(limit, 16<<10))
	return computer.TerminalOutput{Text: text, Cursor: next, Truncated: truncated, HasMore: next < terminal.output.End, Exited: terminal.exited, ExitCode: terminal.exitCode, ErrorCode: terminal.errorCode}, nil
}

func (terminal *managedTerminal) readOutput() {
	defer close(terminal.readDone)
	chunk := make([]byte, 8192)
	pending := make([]byte, 0, len(chunk)+utf8.UTFMax)
	for {
		count, err := terminal.master.Read(chunk)
		pending = append(pending, chunk[:count]...)
		complete := 0
		for complete < len(pending) && utf8.FullRune(pending[complete:]) {
			_, size := utf8.DecodeRune(pending[complete:])
			complete += size
		}
		if err != nil {
			complete = len(pending)
		}
		terminal.mu.Lock()
		if complete > 0 {
			terminal.output.Append([]byte(strings.ToValidUTF8(string(pending[:complete]), "�")))
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) && !errors.Is(err, os.ErrClosed) {
			terminal.errorCode = "terminal_read_failed"
		}
		terminal.mu.Unlock()
		pending = append(pending[:0], pending[complete:]...)
		if err != nil {
			return
		}
	}
}

func (terminal *managedTerminal) wait() {
	err := terminal.command.Wait()
	// 直接程序结束后给尾部输出留出有界排空时间。关闭本终端不会向其他终端或整个桌面发信号。
	timer := time.NewTimer(time.Second)
	select {
	case <-terminal.readDone:
	case <-timer.C:
	}
	timer.Stop()
	terminal.closeOnce.Do(func() { terminal.closeError = terminal.master.Close() })
	<-terminal.readDone
	terminal.mu.Lock()
	defer terminal.mu.Unlock()
	terminal.exited = true
	if terminal.command.ProcessState != nil {
		terminal.exitCode = terminal.command.ProcessState.ExitCode()
	}
	var exit *exec.ExitError
	if terminal.closeError != nil || err != nil && !errors.As(err, &exit) {
		terminal.errorCode = "terminal_wait_failed"
	}
	close(terminal.done)
}

// write 保留最近一次的输入摘要与实际写入字节数，部分失败和回包丢失都不能触发自动重发。
func (terminal *managedTerminal) write(ctx context.Context, sequence int64, text string) (computer.TerminalReceipt, error) {
	terminal.input.Lock()
	defer terminal.input.Unlock()
	digest := sha256.Sum256([]byte(text))
	if sequence == terminal.receipt.Sequence {
		if digest != terminal.lastInput {
			return computer.TerminalReceipt{}, errors.New("终端输入序号与已有内容冲突")
		}
		return terminal.receipt, nil
	}
	if sequence != terminal.receipt.Sequence+1 {
		return computer.TerminalReceipt{}, errors.New("终端输入序号过期或不连续")
	}
	if err := ctx.Err(); err != nil {
		return computer.TerminalReceipt{}, err
	}
	terminal.mu.Lock()
	exited := terminal.exited
	terminal.mu.Unlock()
	if exited {
		return computer.TerminalReceipt{}, errors.New("终端程序已退出")
	}
	operation, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	deadline, _ := operation.Deadline()
	if terminal.master.SetWriteDeadline(deadline) != nil {
		return computer.TerminalReceipt{}, errors.New("设置终端写入期限失败")
	}
	canceled := make(chan struct{})
	var cancelError error
	stop := context.AfterFunc(operation, func() { cancelError = terminal.master.SetWriteDeadline(time.Now()); close(canceled) })
	count, err := io.WriteString(terminal.master, text)
	if !stop() {
		<-canceled
	}
	receipt := computer.TerminalReceipt{Sequence: sequence, WrittenBytes: count}
	if err != nil || cancelError != nil || count != len(text) {
		receipt.ErrorCode = "terminal_write_incomplete"
	}
	terminal.lastInput, terminal.receipt = digest, receipt
	return receipt, nil
}

// closeAfterDrain 只回收描述符和直接子进程；调用方已经撤销授权并确认资源组为空。
// 仍未完成引导的子进程也必须结束，不能用一次空组观测跳过等待。
func (terminal *managedTerminal) closeAfterDrain(ctx context.Context) error {
	terminal.closeOnce.Do(func() { terminal.closeError = terminal.master.Close() })
	for _, done := range []<-chan struct{}{terminal.done, terminal.readDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.New("终端引导或输出回收尚未完成")
		}
	}
	if terminal.closeError != nil {
		return errors.New("关闭终端设备失败")
	}
	return nil
}
