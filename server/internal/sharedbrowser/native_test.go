package sharedbrowser

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/computer"
)

type testNativeDesktop struct {
	sessions []computer.ExecutionSession
	called   bool
	session  computer.ExecutionSession
	window   int64
	command  Command
	failure  error
}

func (*testNativeDesktop) HumanControl(context.Context, computer.ExecutionSession, bool) error {
	return ErrUnavailable
}

func (*testNativeDesktop) Bind(context.Context, computer.ExecutionSession, int64, [4]int) error {
	return ErrUnavailable
}
func (native *testNativeDesktop) Execute(_ context.Context, session computer.ExecutionSession, window int64, command Command) error {
	native.called, native.session, native.window, native.command = true, session, window, command
	return native.failure
}
func (*testNativeDesktop) Screenshot(context.Context, computer.ExecutionSession, int64) ([]byte, error) {
	return nil, ErrUnavailable
}
func (*testNativeDesktop) Release(context.Context, computer.ExecutionSession) error {
	return ErrUnavailable
}

func TestBrowserInputUsesOwnedNativeDesktop(t *testing.T) {
	options, _, sessions := testOptions()
	native := &testNativeDesktop{}
	options.Native = native
	service := &Service{options: options}
	item := &entry{session: sessions[0], nativeID: 17}
	for _, command := range []Command{{Sequence: 1, Kind: "click", X: 500, Y: 300}, {Sequence: 2, Kind: "type", Text: "桌面输入"}, {Sequence: 3, Kind: "navigate", URL: "https://example.com"}} {
		if err := service.command(t.Context(), item, command); err != nil || !native.called || native.session != sessions[0] || native.window != 17 || native.command != command {
			t.Fatal("浏览器未使用所属 Bot 的原生桌面输入", err)
		}
	}
	native.failure = ErrUnavailable
	if err := service.command(t.Context(), item, Command{Sequence: 4, Kind: "reload"}); err != ErrUnavailable {
		t.Fatal("桌面失败未传回调用方", err)
	}
}
