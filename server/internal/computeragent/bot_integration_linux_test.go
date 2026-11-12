//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/provider"
)

type scopedAgentRuntime struct {
	*kernelScope
	reference processgroup.Reference
}

type connectedAgentScope struct {
	*fakeScope
	runtime scopedAgentRuntime
}

func (scope *connectedAgentScope) StartAgent(ctx context.Context, _ processgroup.Reference, session computer.ExecutionSession, _, _ *os.File) error {
	return scope.runtime.StartAgent(ctx, session)
}
func (scope *connectedAgentScope) WriteAgent(ctx context.Context, request computer.AgentWrite) (int64, error) {
	return scope.runtime.WriteAgent(ctx, request)
}
func (scope *connectedAgentScope) ReadAgent(ctx context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	return scope.runtime.ReadAgent(ctx, request)
}

func (runtime scopedAgentRuntime) StartAgent(ctx context.Context, session computer.ExecutionSession) error {
	return runtime.kernelScope.StartAgent(ctx, runtime.reference, session, nil, nil)
}

// testManagedBot 直接降权启动真实程序，验证监督与分块链路；生产入组仍须固定引导和内核联合验收。
func testManagedBot(t *testing.T, reference processgroup.Reference) scopedAgentRuntime {
	t.Helper()
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, reference.Session)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/usr/lib/tombot/bot-agent")
	command.Dir, command.Env = "/workspace", []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(reference.Session.UID), Gid: uint32(reference.Session.GID)}}
	agent, err := launchAgent(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	scope := &kernelScope{directory: directory, membershipUsed: true, agent: &agentAttempt{agent: agent}}
	t.Cleanup(func() {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case <-agent.done:
		case <-time.After(2 * time.Second):
			t.Error("测试 Agent 未退出")
			return
		}
		scope.drained = true
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := scope.Close(); err == nil {
		t.Fatal("未确认回收前清理 Agent 身份目录")
	}
	return scopedAgentRuntime{kernelScope: scope, reference: reference}
}

type containerAgentRelay struct{ models, tools int }

func (relay *containerAgentRelay) Model(_ context.Context, key string, input modelgateway.Input) (botagent.ModelOutput, error) {
	relay.models++
	if len(input.Messages[0].Blocks[0].Text) != 70<<10 {
		return botagent.ModelOutput{}, errors.New("大上下文分块传输损坏")
	}
	if key == "model.1" {
		return botagent.ModelOutput{StopReason: "tool_calls", Tools: []botagent.ToolCall{{ID: "read-one", Name: "file_read", Arguments: `{"number":9007199254740993,"path":"/workspace/shared"}`}}}, nil
	}
	if key != "model.2" || len(input.Messages) != 3 || input.Messages[2].Blocks[0].ToolResult.Content != "共享内容" {
		return botagent.ModelOutput{}, errors.New("Agent 循环上下文丢失")
	}
	return botagent.ModelOutput{Text: "已核对共享内容。", StopReason: "stop"}, nil
}
func (relay *containerAgentRelay) Tool(_ context.Context, key string, call botagent.ToolCall) (botagent.ToolResult, error) {
	relay.tools++
	if key != "tool.1.1" || call.Arguments != `{"number":9007199254740993,"path":"/workspace/shared"}` {
		return botagent.ToolResult{}, errors.New("工具参数分块改变")
	}
	return botagent.ToolResult{Content: "共享内容"}, nil
}

func TestKernelManagedAgentDispatch(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("必须在专用镜像中验收 Agent")
	}
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	first := testManagedBot(t, fixture.reference)
	if _, err := fixture.controller.Open(context.Background(), fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	fixture.guard.scope = &connectedAgentScope{fakeScope: fixture.factory.scopes[0], runtime: first}
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, time.Second, 8, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan error, 128)
	var pending sync.WaitGroup
	client, err := NewClient(func(ctx context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		pending.Add(1)
		go func() { defer pending.Done(); requests <- server.serveConnection(ctx, right) }()
		return left, nil
	}, sameTestProcess, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, other := desktopCommand(fixture.command, 2)
	second := testManagedBot(t, other)
	if first.agent.agent.command.Process.Pid == second.agent.agent.command.Process.Pid {
		t.Fatal("两个 Bot 共用进程")
	}
	start := botagent.Start{Input: modelgateway.Input{System: "Complete the user's task.", Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: strings.Repeat("x", 70<<10)}}}}, Tools: []provider.ToolSchema{{Name: "file_read", Description: "Read a shared file.", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, Limits: botagent.Limits{ModelSteps: 4, ToolCalls: 4, OutputBytes: 4096, ContextBytes: 128 << 10}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 2)
	for index, runtime := range []scopedAgentRuntime{first, second} {
		go func() {
			relay := &containerAgentRelay{}
			var target computer.AgentRuntime = runtime
			if index == 0 {
				target = client
			}
			answer, err := botagent.Dispatch(ctx, target, runtime.reference.Session, start, relay, time.Second)
			if err == nil && (answer != "已核对共享内容。" || relay.models != 2 || relay.tools != 1) {
				err = errors.New("真实进程未完成完整循环")
			}
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	pending.Wait()
	for len(requests) > 0 {
		if err := <-requests; err != nil {
			t.Fatal("真实进程分块协议未确认", err)
		}
	}
	// 已完成会话的重复启动保留同一进程记录，不重新执行用户运行。
	pid := first.agent.agent.command.Process.Pid
	if err := first.StartAgent(ctx, first.reference.Session); err != nil || first.agent.agent.command.Process.Pid != pid {
		t.Fatal("重复启动替换了 Agent", err)
	}
	wrong := first.reference.Session
	wrong.FencingToken++
	if _, err := first.ReadAgent(ctx, computer.AgentRead{Session: wrong}); err == nil {
		t.Fatal("另一代次读取了 Agent")
	}
}

func TestKernelAgentBootstrapRejectsFakeMembership(t *testing.T) {
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, fixture.command.Session)
	if err != nil {
		t.Fatal(err)
	}
	membership, err := os.CreateTemp(t.TempDir(), "invalid-membership-")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(membership.Close(), reader.Close(), writer.Close()); err != nil {
			t.Error(err)
		}
	}()
	scope := &kernelScope{directory: directory, membershipUsed: true}
	if err := scope.StartAgent(context.Background(), fixture.reference, fixture.command.Session, membership, reader); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scope.agent.agent.done:
	case <-time.After(3 * time.Second):
		t.Fatal("伪造入组仍运行了 Agent")
	}
	output, err := scope.ReadAgent(context.Background(), computer.AgentRead{Session: fixture.command.Session})
	if err != nil || !output.Exited || output.ExitCode != 125 || len(output.Bytes) != 0 {
		t.Fatal("固定引导未拒绝伪造入组", output, err)
	}
	first := scope.agent.agent
	if err := scope.StartAgent(context.Background(), fixture.reference, fixture.command.Session, membership, reader); err != nil || scope.agent.agent != first {
		t.Fatal("失败启动被重放", err)
	}
	scope.drained = true
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}
