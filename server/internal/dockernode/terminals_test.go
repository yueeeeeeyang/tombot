package dockernode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type fakeTerminalAuthority struct {
	*fakeAuthority
	client      *fakeTerminalRuntime
	connections int
}

func (authority *fakeTerminalAuthority) terminalClient(_ context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.TerminalRuntime, error) {
	if reference == nil || reference.Session != command.Session {
		return nil, errors.New("测试终端资源组身份不符")
	}
	authority.connections++
	return authority.client, nil
}

type fakeTerminalRuntime struct {
	mu       sync.Mutex
	calls    int
	fail     bool
	deadline time.Time
}

func (runtime *fakeTerminalRuntime) call(ctx context.Context) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.calls++
	runtime.deadline, _ = ctx.Deadline()
	if runtime.fail {
		return errors.New("测试终端响应未知")
	}
	return nil
}
func (runtime *fakeTerminalRuntime) StartTerminal(ctx context.Context, _ computer.TerminalStart) (computer.TerminalOutput, error) {
	return computer.TerminalOutput{ExitCode: -1}, runtime.call(ctx)
}
func (runtime *fakeTerminalRuntime) ReadTerminal(ctx context.Context, _ computer.TerminalRead) (computer.TerminalOutput, error) {
	return computer.TerminalOutput{ExitCode: -1}, runtime.call(ctx)
}
func (runtime *fakeTerminalRuntime) WriteTerminal(ctx context.Context, request computer.TerminalWrite) (computer.TerminalReceipt, error) {
	return computer.TerminalReceipt{Sequence: request.Sequence, WrittenBytes: len(request.Text)}, runtime.call(ctx)
}
func (runtime *fakeTerminalRuntime) ResizeTerminal(ctx context.Context, _ computer.TerminalResize) error {
	return runtime.call(ctx)
}

func TestNodeTerminalAuthorizationAndNoReplay(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	controller := fixture.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
	authority := &fakeTerminalAuthority{fakeAuthority: controller.agent.(*fakeAuthority), client: &fakeTerminalRuntime{}}
	controller.agent = authority
	ctx := context.Background()
	request := computer.TerminalStart{TerminalAddress: computer.TerminalAddress{Session: session, OperationID: ids.New()}, Command: computer.ProcessCommand{Program: "/bin/bash", WorkingDirectory: "/workspace"}, Columns: 80, Rows: 24}
	if _, err := controller.StartTerminal(ctx, request); err == nil {
		t.Fatal("未授权终端启动成功")
	}
	command := commandFor(session)
	if _, err := controller.Open(ctx, command); err != nil {
		t.Fatal(err)
	}
	lease, _ := controller.lease(sessionKeyOf(session))
	if _, err := controller.StartTerminal(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ReadTerminal(ctx, computer.TerminalRead{TerminalAddress: request.TerminalAddress, MaximumBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.WriteTerminal(ctx, computer.TerminalWrite{TerminalAddress: request.TerminalAddress, Sequence: 1, Text: "你好\n"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.ResizeTerminal(ctx, computer.TerminalResize{TerminalAddress: request.TerminalAddress, Columns: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	if authority.client.calls != 4 || authority.client.deadline.After(lease.deadline) {
		t.Fatal("终端没有受原始期限约束")
	}
	for _, mutate := range []func(*computer.ExecutionSession){
		func(s *computer.ExecutionSession) { s.UserID = ids.New() }, func(s *computer.ExecutionSession) { s.RunID = ids.New() }, func(s *computer.ExecutionSession) { s.FencingToken++ }, func(s *computer.ExecutionSession) { s.DesktopID = ids.New() },
	} {
		wrong := request
		mutate(&wrong.Session)
		if _, err := controller.StartTerminal(ctx, wrong); err == nil {
			t.Fatal("错误身份获得终端控制权")
		}
	}
	if authority.connections != 4 {
		t.Fatal("拒绝请求仍连接了代理")
	}
	authority.client.fail = true
	if _, err := controller.WriteTerminal(ctx, computer.TerminalWrite{TerminalAddress: request.TerminalAddress, Sequence: 2, Text: "unknown\n"}); err == nil {
		t.Fatal("未知输入结果被报告为成功")
	}
	if authority.client.calls != 5 {
		t.Fatal("未知操作被自动重放")
	}
	lease.deadline = time.Now().Add(-time.Second)
	controller.remember(sessionKeyOf(session), lease)
	if _, err := controller.ReadTerminal(ctx, computer.TerminalRead{TerminalAddress: request.TerminalAddress, MaximumBytes: 4096}); err == nil {
		t.Fatal("过期会话仍可读取终端")
	}
	if authority.client.calls != 5 {
		t.Fatal("过期操作仍送给代理")
	}
}
