package computer

import (
	"slices"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func desktopIOSession() ExecutionSession {
	return ExecutionSession{DesktopIdentity: DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: DesktopUIDBase + 1, GID: DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
}

func TestDesktopInputValidation(t *testing.T) {
	request := DesktopInput{Session: desktopIOSession(), Sequence: 1, Events: []DesktopEvent{
		{Kind: "pointer", X: 1279, Y: 799, Buttons: 1}, {Kind: "pointer", X: 100, Y: 100},
		{Kind: "key", Key: 0xffe3, Down: true}, {Kind: "key", Key: 'a', Down: true}, {Kind: "key", Key: 'a'}, {Kind: "key", Key: 0xffe3},
		{Kind: "key", Key: 0x01004e2d, Down: true}, {Kind: "key", Key: 0x01004e2d},
	}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*DesktopInput){
		"无会话": func(r *DesktopInput) { r.Session.SessionID = "" }, "无序号": func(r *DesktopInput) { r.Sequence = 0 },
		"空事件": func(r *DesktopInput) { r.Events = nil }, "超量": func(r *DesktopInput) { r.Events = make([]DesktopEvent, MaximumDesktopEvents+1) },
		"越界": func(r *DesktopInput) { r.Events[0].X = 1280 }, "负坐标": func(r *DesktopInput) { r.Events[0].Y = -1 },
		"未知动作": func(r *DesktopInput) { r.Events[0].Kind = "clipboard" }, "额外键": func(r *DesktopInput) { r.Events[0].Key = 1 },
		"未释放鼠标": func(r *DesktopInput) { r.Events[1].Buttons = 1 }, "未释放键": func(r *DesktopInput) { r.Events = r.Events[:7] },
		"重复按下": func(r *DesktopInput) { r.Events[4].Down = true }, "先释放": func(r *DesktopInput) { r.Events[2].Down = false },
		"键盘携带鼠标": func(r *DesktopInput) { r.Events[2].X = 1 }, "无效码点": func(r *DesktopInput) { r.Events[6].Key = 0x0100d800 },
		"零按键": func(r *DesktopInput) { r.Events[6].Key = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := request
			invalid.Events = slices.Clone(request.Events)
			change(&invalid)
			if invalid.Validate() == nil {
				t.Fatal("接受无效输入")
			}
		})
	}
	for _, state := range []string{"sent", "uncertain"} {
		if err := (DesktopInputReceipt{Session: request.Session, Sequence: 1, State: state}).Validate(request); err != nil {
			t.Fatal(err)
		}
	}
	if (DesktopInputReceipt{Session: request.Session, Sequence: 1, State: "applied"}).Validate(request) == nil {
		t.Fatal("把传输当成应用执行确认")
	}
}

func TestDesktopImageBoundaries(t *testing.T) {
	request := DesktopCapture{Session: desktopIOSession(), Sequence: 1}
	result := DesktopImage{DesktopCapture: request, Width: DesktopWidth, Height: DesktopHeight, MediaType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64)}
	if err := result.Validate(request); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DesktopImage){func(v *DesktopImage) { v.Session.BotID = ids.New() }, func(v *DesktopImage) { v.Sequence++ }, func(v *DesktopImage) { v.Width++ }, func(v *DesktopImage) { v.Height++ }, func(v *DesktopImage) { v.Size = MaximumDesktopImageBytes + 1 }, func(v *DesktopImage) { v.MediaType = "text/plain" }, func(v *DesktopImage) { v.SHA256 = "abc" }} {
		invalid := result
		mutate(&invalid)
		if invalid.Validate(request) == nil {
			t.Fatal("接受无效图像")
		}
	}
	read := DesktopImageRead{DesktopCapture: request, Offset: 50, MaximumBytes: 30}
	chunk := DesktopImageChunk{Image: result, Offset: 50, Bytes: make([]byte, 30)}
	if err := chunk.Validate(read); err != nil {
		t.Fatal(err)
	}
	chunk.Bytes = chunk.Bytes[:29]
	if chunk.Validate(read) == nil {
		t.Fatal("接受短分块")
	}
	read.Offset, chunk.Offset, chunk.Bytes = 100, 100, nil
	if err := chunk.Validate(read); err != nil {
		t.Fatal("正确结束位置被拒绝", err)
	}
	read.Offset, chunk.Offset = 101, 101
	if chunk.Validate(read) == nil {
		t.Fatal("接受越过结束位置的空数据")
	}
	read.MaximumBytes = MaximumDesktopChunkBytes + 1
	if read.Validate() == nil {
		t.Fatal("接受超量读取")
	}
}
