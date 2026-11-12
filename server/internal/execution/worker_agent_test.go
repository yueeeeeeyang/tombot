package execution

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
)

// workerAgents 只验证 Worker 的远端协议与 PostgreSQL 事务，真实二进制和入组另由容器验收。
type workerAgents struct {
	mu        sync.Mutex
	processes map[computer.ExecutionSession]*workerAgent
	t         *testing.T
	starts    int
	failStart bool
	local     botagent.LocalExecutor
}
type workerAgent struct {
	connection              net.Conn
	readOffset, writeOffset int64
	done                    chan struct{}
	exitCode                int
}

func newWorkerAgents(t *testing.T) *workerAgents {
	return &workerAgents{processes: map[computer.ExecutionSession]*workerAgent{}, t: t}
}

func (runtime *workerAgents) StartAgent(ctx context.Context, session computer.ExecutionSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.starts++
	if runtime.failStart {
		return errors.New("测试 Agent 启动未确认")
	}
	if runtime.processes[session] != nil {
		return errors.New("测试运行重复启动 Agent")
	}
	parent, child := net.Pipe()
	agent := &workerAgent{connection: parent, done: make(chan struct{})}
	runtime.processes[session] = agent
	childContext, cancel := context.WithCancel(context.Background())
	go func() {
		err := botagent.Serve(childContext, child, runtime.local)
		if err != nil {
			agent.exitCode = 1
		}
		close(agent.done)
	}()
	runtime.t.Cleanup(func() {
		cancel()
		if err := parent.Close(); err != nil {
			runtime.t.Error(err)
		}
		select {
		case <-agent.done:
		case <-time.After(time.Second):
			runtime.t.Error("测试 Agent 未停止")
		}
	})
	return nil
}

func (runtime *workerAgents) process(session computer.ExecutionSession) (*workerAgent, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	agent := runtime.processes[session]
	if agent == nil {
		return nil, errors.New("测试 Agent 归属不符")
	}
	return agent, nil
}

func (runtime *workerAgents) WriteAgent(ctx context.Context, request computer.AgentWrite) (int64, error) {
	if err := request.Validate(); err != nil {
		return 0, err
	}
	agent, err := runtime.process(request.Session)
	if err != nil {
		return 0, err
	}
	if request.Offset != agent.writeOffset {
		return 0, errors.New("测试 Agent 输入错序")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, errors.New("测试 Agent 缺少输入期限")
	}
	if err = agent.connection.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	count, err := agent.connection.Write(request.Bytes)
	agent.writeOffset += int64(count)
	return agent.writeOffset, err
}

func (runtime *workerAgents) ReadAgent(ctx context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.AgentOutput{}, err
	}
	agent, err := runtime.process(request.Session)
	if err != nil {
		return computer.AgentOutput{}, err
	}
	if request.Offset != agent.readOffset {
		return computer.AgentOutput{}, errors.New("测试 Agent 输出错序")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return computer.AgentOutput{}, errors.New("测试 Agent 缺少输出期限")
	}
	if err = agent.connection.SetReadDeadline(deadline); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return computer.AgentOutput{}, err
	}
	buffer := make([]byte, computer.AgentChunkBytes)
	count, err := agent.connection.Read(buffer)
	agent.readOffset += int64(count)
	output := computer.AgentOutput{Offset: agent.readOffset, Bytes: buffer[:count], ExitCode: -1}
	if errors.Is(err, io.EOF) {
		select {
		case <-agent.done:
			output.Exited, output.ExitCode = true, agent.exitCode
		default:
		}
		return output, nil
	}
	return output, err
}
