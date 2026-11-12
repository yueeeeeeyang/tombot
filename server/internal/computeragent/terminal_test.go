package computeragent

import (
	"context"
	"errors"
	"os"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type recordingTerminalScope struct {
	*fakeScope
	calls   int
	address computer.TerminalAddress
}

func (scope *recordingTerminalScope) StartTerminal(_ context.Context, reference processgroup.Reference, request computer.TerminalStart, membership, revocation *os.File) (computer.TerminalOutput, error) {
	if reference.Session != request.Session || membership == nil || revocation == nil {
		return computer.TerminalOutput{}, errors.New("测试终端没有授权入口")
	}
	scope.calls++
	scope.address = request.TerminalAddress
	return computer.TerminalOutput{Text: "中文", Cursor: 6, ExitCode: -1}, nil
}
func (scope *recordingTerminalScope) ReadTerminal(_ context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
	if request.TerminalAddress != scope.address {
		return computer.TerminalOutput{}, errors.New("测试终端归属不符")
	}
	scope.calls++
	return computer.TerminalOutput{Text: "中文", Cursor: 6, ExitCode: -1}, nil
}
func (scope *recordingTerminalScope) WriteTerminal(_ context.Context, request computer.TerminalWrite) (computer.TerminalReceipt, error) {
	if request.TerminalAddress != scope.address {
		return computer.TerminalReceipt{}, errors.New("测试终端归属不符")
	}
	scope.calls++
	return computer.TerminalReceipt{Sequence: request.Sequence, WrittenBytes: len(request.Text)}, nil
}
func (scope *recordingTerminalScope) ResizeTerminal(_ context.Context, request computer.TerminalResize) error {
	if request.TerminalAddress != scope.address {
		return errors.New("测试终端归属不符")
	}
	scope.calls++
	return nil
}

func terminalFixture(t *testing.T) (*fixture, *recordingTerminalScope, computer.TerminalStart) {
	t.Helper()
	fixture := newFixture(t)
	if _, err := fixture.controller.Open(context.Background(), fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingTerminalScope{fakeScope: fixture.factory.scopes[0]}
	fixture.guard.scope = scope
	request := computer.TerminalStart{TerminalAddress: computer.TerminalAddress{Session: fixture.command.Session, OperationID: ids.New()}, Command: computer.ProcessCommand{Program: "/bin/bash", WorkingDirectory: "/workspace", Arguments: []string{"--noprofile", "--norc", "-i"}}, Columns: 80, Rows: 24}
	return &fixture, scope, request
}

func TestTerminalControllerAuthorization(t *testing.T) {
	fixture, scope, request := terminalFixture(t)
	ctx := context.Background()
	if _, err := fixture.controller.StartTerminal(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controller.ReadTerminal(ctx, computer.TerminalRead{TerminalAddress: request.TerminalAddress, MaximumBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controller.WriteTerminal(ctx, computer.TerminalWrite{TerminalAddress: request.TerminalAddress, Sequence: 1, Text: "你好\n"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controller.ResizeTerminal(ctx, computer.TerminalResize{TerminalAddress: request.TerminalAddress, Columns: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	if scope.calls != 4 {
		t.Fatal("终端正常路径未完整调用", scope.calls)
	}
	for _, mutate := range []func(*computer.TerminalStart){
		func(r *computer.TerminalStart) { r.Session.UserID = ids.New() },
		func(r *computer.TerminalStart) { r.Session.RunID = ids.New() },
		func(r *computer.TerminalStart) { r.Session.FencingToken++ },
		func(r *computer.TerminalStart) { r.Session.BotID = ids.New() },
		func(r *computer.TerminalStart) { r.Session.DesktopID = ids.New() },
		func(r *computer.TerminalStart) { r.Session.UID++ },
		func(r *computer.TerminalStart) { r.Command.Environment = map[string]string{"DISPLAY": ":2"} },
	} {
		wrong := request
		mutate(&wrong)
		if _, err := fixture.controller.StartTerminal(ctx, wrong); err == nil {
			t.Fatal("错误桌面身份或受管环境得到执行权限")
		}
	}
	if scope.calls != 4 {
		t.Fatal("无效请求进入了终端执行器")
	}
	if err := fixture.guard.permit.revoke(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controller.WriteTerminal(ctx, computer.TerminalWrite{TerminalAddress: request.TerminalAddress, Sequence: 2, Text: "late"}); err == nil {
		t.Fatal("撤销后仍可输入")
	}
	if scope.calls != 4 {
		t.Fatal("撤销后执行器收到输入")
	}
}

func TestTerminalOutputValidation(t *testing.T) {
	for _, output := range []computer.TerminalOutput{
		{Text: "中文", Cursor: 5, ExitCode: -1}, {Cursor: -1, ExitCode: -1}, {ExitCode: 0}, {ExitCode: -1, ErrorCode: "arbitrary"}, {Text: string([]byte{0xff}), Cursor: 1, ExitCode: -1},
	} {
		if _, err := validateTerminalOutput(terminalResult{Output: &output}, 0, 4096); err == nil {
			t.Fatal("接受了错误输出", output)
		}
	}
	output := computer.TerminalOutput{Text: "中文", Cursor: 16, Truncated: true, ExitCode: -1}
	if _, err := validateTerminalOutput(terminalResult{Output: &output}, 0, 4096); err != nil {
		t.Fatal(err)
	}
}
