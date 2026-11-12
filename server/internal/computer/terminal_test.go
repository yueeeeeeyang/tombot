package computer

import (
	"math"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func TestTerminalOutputCursorAndExitEvidence(t *testing.T) {
	for _, output := range []TerminalOutput{
		{Text: "中文", Cursor: 16, ExitCode: -1},
		{Text: "中文", Cursor: 30, Truncated: true, ExitCode: -1},
		{Cursor: 10, Exited: true, ExitCode: 0},
		{Text: "中文", Cursor: 16, Exited: true, ExitCode: 9, HasMore: true},
		{Cursor: 10, Exited: true, ExitCode: -1},
		{Cursor: 10, ErrorCode: "terminal_read_failed", ExitCode: -1},
	} {
		if err := output.Validate(10, 64); err != nil {
			t.Fatal("有效输出被拒绝", output, err)
		}
	}
	for _, output := range []TerminalOutput{
		{Text: "中文", Cursor: 15, Truncated: true, ExitCode: -1},
		{Text: "中文", Cursor: 17, ExitCode: -1},
		{Cursor: 9, Truncated: true, ExitCode: -1},
		{Cursor: 10, HasMore: true, ExitCode: -1},
		{Text: "\xff", Cursor: 11, ExitCode: -1},
		{Cursor: 10, ExitCode: 0},
		{Cursor: 10, Exited: true, ExitCode: 256},
		{Cursor: 10, Exited: true, ExitCode: -2},
		{Cursor: 10, ErrorCode: "unknown", ExitCode: -1},
	} {
		if err := output.Validate(10, 64); err == nil {
			t.Fatal("接受伪造输出或状态", output)
		}
	}
	if (TerminalOutput{Text: "a", Cursor: math.MaxInt64, ExitCode: -1}).Validate(math.MaxInt64, 64) == nil {
		t.Fatal("游标溢出仍接受输出")
	}
	for _, maximum := range []int{0, 3, MaximumTerminalReadBytes + 1} {
		if (TerminalOutput{ExitCode: -1}).Validate(0, maximum) == nil {
			t.Fatal("接受无效输出上限")
		}
	}
	if (TerminalOutput{ExitCode: -1}).Validate(-1, 64) == nil {
		t.Fatal("接受负请求游标")
	}
	if (TerminalOutput{Text: "12345", Cursor: 5, ExitCode: -1}).Validate(0, 4) == nil {
		t.Fatal("接受超限输出")
	}
}

func TestTerminalRequestLimits(t *testing.T) {
	// 此处只测试新增的输入边界；完整桌面身份沿用 ExecutionSession 的验证。
	for _, size := range [][2]int{{2, 2}, {500, 300}, {80, 24}} {
		if err := ValidateTerminalSize(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range [][2]int{{1, 24}, {501, 24}, {80, 1}, {80, 301}} {
		if err := ValidateTerminalSize(size[0], size[1]); err == nil {
			t.Fatal("接受了越界尺寸", size)
		}
	}
	address := TerminalAddress{OperationID: ids.New()}
	if err := address.Validate(); err == nil {
		t.Fatal("接受了缺失身份")
	}
	address.Session = ExecutionSession{DesktopIdentity: DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: DesktopUIDBase + 1, GID: DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	if err := address.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, maximum := range []int{4, MaximumTerminalReadBytes} {
		if err := (TerminalRead{TerminalAddress: address, MaximumBytes: maximum}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, maximum := range []int{0, 3, MaximumTerminalReadBytes + 1} {
		if err := (TerminalRead{TerminalAddress: address, MaximumBytes: maximum}).Validate(); err == nil {
			t.Fatal("接受了无效读取上限")
		}
	}
	if err := (TerminalRead{TerminalAddress: address, Cursor: -1, MaximumBytes: 4}).Validate(); err == nil {
		t.Fatal("接受了负游标")
	}
	for _, request := range []TerminalWrite{
		{Sequence: 0, Text: "abc"}, {Sequence: 1, Text: ""}, {Sequence: 1, Text: strings.Repeat("x", MaximumTerminalInputBytes+1)}, {Sequence: 1, Text: string([]byte{0xff})},
	} {
		request.TerminalAddress = address
		if request.Validate() == nil {
			t.Fatal("接受了无效输入")
		}
	}
	for _, text := range []string{"中文\n", strings.Repeat("x", MaximumTerminalInputBytes)} {
		if err := (TerminalWrite{TerminalAddress: address, Sequence: 1, Text: text}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
