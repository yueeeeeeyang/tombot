package computeragent

import (
	"context"
	"errors"
	"os"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type recordingBotScope struct {
	*fakeScope
	starts    int
	input     []byte
	badOutput bool
}

func (scope *recordingBotScope) StartAgent(_ context.Context, reference processgroup.Reference, session computer.ExecutionSession, membership, revocation *os.File) error {
	if reference.Session != session || membership == nil || revocation == nil {
		return errors.New("测试 Agent 缺少授权描述符")
	}
	scope.starts++
	return nil
}
func (scope *recordingBotScope) WriteAgent(_ context.Context, request computer.AgentWrite) (int64, error) {
	if request.Offset != int64(len(scope.input)) {
		return 0, errors.New("测试 Agent 输入错序")
	}
	scope.input = append(scope.input, request.Bytes...)
	return int64(len(scope.input)), nil
}
func (scope *recordingBotScope) ReadAgent(_ context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	output := computer.AgentOutput{Offset: request.Offset, ExitCode: -1}
	if request.Offset < int64(len(scope.input)) {
		output.Bytes = scope.input[request.Offset:]
		output.Offset = int64(len(scope.input))
	}
	if scope.badOutput {
		output.Offset++
	}
	return output, nil
}

func TestBotAgentAuthorization(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.controller.StartAgent(ctx, fixture.command.Session); err == nil {
		t.Fatal("未授权即可启动 Agent")
	}
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingBotScope{fakeScope: fixture.factory.scopes[0]}
	fixture.guard.scope = scope
	if err := fixture.controller.StartAgent(ctx, fixture.command.Session); err != nil {
		t.Fatal(err)
	}
	wrong := fixture.command.Session
	wrong.FencingToken++
	if err := fixture.controller.StartAgent(ctx, wrong); err == nil || scope.starts != 1 {
		t.Fatal("错误代次进入 Agent 启动器", err)
	}
	request := computer.AgentWrite{Session: fixture.command.Session, Bytes: []byte("协议内容")}
	if _, err := fixture.controller.WriteAgent(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := fixture.guard.permit.revoke(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controller.ReadAgent(ctx, computer.AgentRead{Session: fixture.command.Session}); err == nil {
		t.Fatal("撤销后仍读取 Agent 内容")
	}
	if _, err := fixture.controller.WriteAgent(ctx, request); err == nil {
		t.Fatal("撤销后仍向 Agent 输入")
	}
}
