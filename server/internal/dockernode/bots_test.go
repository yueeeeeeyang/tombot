package dockernode

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type fakeBotRuntime struct{ *fakeTerminalRuntime }

func (runtime *fakeBotRuntime) StartAgent(ctx context.Context, _ computer.ExecutionSession) error {
	return runtime.call(ctx)
}
func (runtime *fakeBotRuntime) WriteAgent(ctx context.Context, request computer.AgentWrite) (int64, error) {
	return request.Offset + int64(len(request.Bytes)), runtime.call(ctx)
}
func (runtime *fakeBotRuntime) ReadAgent(ctx context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	return computer.AgentOutput{Offset: request.Offset, ExitCode: -1}, runtime.call(ctx)
}

type fakeBotAuthority struct {
	*fakeAuthority
	runtime     *fakeBotRuntime
	connections int
}

func (authority *fakeBotAuthority) agentClient(_ context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.AgentRuntime, error) {
	if reference == nil || reference.Session != command.Session {
		return nil, errors.New("测试 Agent 资源组不符")
	}
	authority.connections++
	return authority.runtime, nil
}

func TestNodeAgentAuthorizationAndNoReplay(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	controller := fixture.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
	authority := &fakeBotAuthority{fakeAuthority: controller.agent.(*fakeAuthority), runtime: &fakeBotRuntime{&fakeTerminalRuntime{}}}
	controller.agent = authority
	ctx := context.Background()
	if err := controller.StartAgent(ctx, session); err == nil {
		t.Fatal("未取得执行权即可启动 Agent")
	}
	if _, err := controller.Open(ctx, commandFor(session)); err != nil {
		t.Fatal(err)
	}
	if err := controller.StartAgent(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.WriteAgent(ctx, computer.AgentWrite{Session: session, Bytes: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ReadAgent(ctx, computer.AgentRead{Session: session}); err != nil {
		t.Fatal(err)
	}
	lease, _ := controller.lease(sessionKeyOf(session))
	if authority.runtime.deadline.After(lease.deadline) {
		t.Fatal("分块调用延长执行权")
	}
	wrong := session
	wrong.FencingToken++
	if _, err := controller.ReadAgent(ctx, computer.AgentRead{Session: wrong}); err == nil || authority.connections != 3 {
		t.Fatal("另一代次读取了 Agent", err)
	}
	authority.runtime.fail = true
	if err := controller.StartAgent(ctx, session); err == nil || authority.runtime.calls != 4 {
		t.Fatal("未知启动被重放或报告成功", err)
	}
	lease.deadline = time.Now().Add(-time.Second)
	controller.remember(sessionKeyOf(session), lease)
	if _, err := controller.WriteAgent(ctx, computer.AgentWrite{Session: session, Bytes: []byte{1}}); err == nil || authority.connections != 4 {
		t.Fatal("过期后仍进入 Agent 输入", err)
	}
}
