//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

type agentAttempt struct {
	agent   *managedAgent
	failure error
}

// managedAgent 只保存上一块回执，背压由私有管道承担，不把整个上下文积存在特权进程内。
type managedAgent struct {
	command                 *exec.Cmd
	input, output           *os.File
	inputMu, outputMu       sync.Mutex
	writeOffset, readOffset int64
	lastWrite               [32]byte
	lastWriteSize           int
	writeFailed             bool
	lastRead                []byte
	eof                     bool
	readFailed              bool
	mu                      sync.Mutex
	exitCode                int
	done                    chan struct{}
	closeOnce               sync.Once
	closeError              error
}

func (scope *kernelScope) StartAgent(ctx context.Context, reference processgroup.Reference, session computer.ExecutionSession, membership, revocation *os.File) error {
	if reference.Validate() != nil || reference.Session != session {
		return errors.New("Bot Agent 资源组身份无效")
	}
	scope.mu.Lock()
	if scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session {
		scope.mu.Unlock()
		return errors.New("Bot Agent 桌面已关闭或身份不符")
	}
	if scope.agent != nil {
		failure := scope.agent.failure
		started := scope.agent.agent != nil
		scope.mu.Unlock()
		if failure != nil {
			return failure
		}
		if !started {
			return errors.New("Bot Agent 启动结果未知")
		}
		return nil
	}
	attempt := &agentAttempt{}
	scope.agent = attempt
	scope.mu.Unlock()
	agent, err := startManagedAgent(ctx, reference, membership, revocation)
	scope.mu.Lock()
	attempt.agent, attempt.failure = agent, err
	scope.mu.Unlock()
	return err
}

func startManagedAgent(ctx context.Context, reference processgroup.Reference, membership, revocation *os.File) (_ *managedAgent, failure error) {
	command, sealed, err := prepareTask(taskprocess.Request{Version: 2, OperationID: reference.Session.SessionID, Group: reference, Role: "shell", ProcessCommand: computer.ProcessCommand{Program: "/usr/lib/tombot/bot-agent", WorkingDirectory: "/workspace"}}, membership, revocation)
	if err != nil {
		return nil, err
	}
	defer func() {
		if sealed.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭 Bot Agent 封存请求失败"))
		}
	}()
	return launchAgent(ctx, command)
}

// launchAgent 的 command 仅由固定任务引导构造；测试以独立非 root 进程验证真实管道生命周期。
func launchAgent(ctx context.Context, command *exec.Cmd) (_ *managedAgent, failure error) {
	childInput, input, err := os.Pipe()
	if err != nil {
		return nil, errors.New("创建 Bot Agent 输入管道失败")
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(errors.New("创建 Bot Agent 输出管道失败"), childInput.Close(), input.Close())
	}
	defer func() { failure = errors.Join(failure, childInput.Close(), childOutput.Close()) }()
	command.Stdin, command.Stdout, command.Stderr = childInput, childOutput, io.Discard
	if err = ctx.Err(); err == nil {
		err = command.Start()
	}
	if err != nil {
		return nil, errors.Join(errors.New("启动受信 Bot Agent 引导失败"), input.Close(), output.Close())
	}
	agent := &managedAgent{command: command, input: input, output: output, done: make(chan struct{}), exitCode: -1}
	go func() {
		err := command.Wait()
		code := 125
		if command.ProcessState != nil {
			code = command.ProcessState.ExitCode()
		}
		if code < 0 || code > 255 || err != nil && code == 0 {
			code = 125
		}
		agent.mu.Lock()
		agent.exitCode = code
		agent.mu.Unlock()
		close(agent.done)
	}()
	return agent, nil
}

func (scope *kernelScope) bot(session computer.ExecutionSession) (*managedAgent, error) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if session.Validate() != nil || scope.closed || scope.revoked || scope.drained || scope.directory == nil || scope.directory.session != session || scope.agent == nil || scope.agent.agent == nil || scope.agent.failure != nil {
		return nil, errors.New("Bot Agent 不存在、已关闭或启动未确认")
	}
	return scope.agent.agent, nil
}

func (scope *kernelScope) WriteAgent(ctx context.Context, request computer.AgentWrite) (int64, error) {
	if err := request.Validate(); err != nil {
		return 0, err
	}
	agent, err := scope.bot(request.Session)
	if err != nil {
		return 0, err
	}
	return agent.write(ctx, request.Offset, request.Bytes)
}

func (scope *kernelScope) ReadAgent(ctx context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.AgentOutput{}, err
	}
	agent, err := scope.bot(request.Session)
	if err != nil {
		return computer.AgentOutput{}, err
	}
	return agent.read(ctx, request.Offset)
}

func ioDeadline(ctx context.Context, maximum time.Duration) time.Time {
	deadline := time.Now().Add(maximum)
	if requested, exists := ctx.Deadline(); exists && requested.Before(deadline) {
		return requested
	}
	return deadline
}

func (agent *managedAgent) write(ctx context.Context, offset int64, chunk []byte) (int64, error) {
	agent.inputMu.Lock()
	defer agent.inputMu.Unlock()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if len(chunk) < 1 || len(chunk) > computer.AgentChunkBytes || agent.writeFailed {
		return 0, errors.New("Bot Agent 输入已失效或大小无效")
	}
	digest := sha256.Sum256(chunk)
	if agent.lastWriteSize > 0 && offset == agent.writeOffset-int64(agent.lastWriteSize) && digest == agent.lastWrite && len(chunk) == agent.lastWriteSize {
		return agent.writeOffset, nil
	}
	if offset != agent.writeOffset {
		return 0, errors.New("Bot Agent 输入偏移冲突")
	}
	if err := agent.input.SetWriteDeadline(ioDeadline(ctx, time.Second)); err != nil {
		return 0, errors.New("设置 Bot Agent 输入期限失败")
	}
	count, err := agent.input.Write(chunk)
	if err != nil || count != len(chunk) {
		agent.writeFailed = true
		return 0, errors.New("Bot Agent 输入结果未知，禁止重发")
	}
	agent.writeOffset += int64(count)
	agent.lastWrite, agent.lastWriteSize = digest, count
	return agent.writeOffset, nil
}

func (agent *managedAgent) read(ctx context.Context, offset int64) (computer.AgentOutput, error) {
	agent.outputMu.Lock()
	defer agent.outputMu.Unlock()
	if ctx.Err() != nil {
		return computer.AgentOutput{}, ctx.Err()
	}
	if agent.readFailed {
		return computer.AgentOutput{}, errors.New("Bot Agent 输出已经失效")
	}
	var chunk []byte
	if len(agent.lastRead) > 0 && offset == agent.readOffset-int64(len(agent.lastRead)) {
		chunk = agent.lastRead
	} else {
		if offset != agent.readOffset {
			return computer.AgentOutput{}, errors.New("Bot Agent 输出偏移冲突")
		}
		if !agent.eof {
			if err := agent.output.SetReadDeadline(ioDeadline(ctx, 100*time.Millisecond)); err != nil {
				return computer.AgentOutput{}, errors.New("设置 Bot Agent 输出期限失败")
			}
			buffer := make([]byte, computer.AgentChunkBytes)
			count, err := agent.output.Read(buffer)
			if errors.Is(err, io.EOF) {
				agent.eof = true
			} else if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
				agent.readFailed = true
				return computer.AgentOutput{}, errors.New("读取 Bot Agent 输出失败")
			}
			if ctx.Err() != nil {
				if count > 0 {
					agent.readFailed = true
				}
				return computer.AgentOutput{}, ctx.Err()
			}
			if count > 0 {
				chunk = buffer[:count]
				agent.lastRead = chunk
				agent.readOffset += int64(count)
			}
		}
	}
	output := computer.AgentOutput{Offset: agent.readOffset, Bytes: bytes.Clone(chunk), ExitCode: -1}
	if agent.eof {
		select {
		case <-agent.done:
			agent.mu.Lock()
			output.Exited, output.ExitCode = true, agent.exitCode
			agent.mu.Unlock()
		default:
		}
	}
	return output, nil
}

func (agent *managedAgent) closeStreams() error {
	agent.closeOnce.Do(func() { agent.closeError = errors.Join(agent.input.Close(), agent.output.Close()) })
	return agent.closeError
}

func (agent *managedAgent) closeAfterDrain(ctx context.Context) error {
	if err := agent.closeStreams(); err != nil {
		return err
	}
	select {
	case <-agent.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
