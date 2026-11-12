package botagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func startPeer(t *testing.T) (net.Conn, <-chan error, context.CancelFunc) {
	t.Helper()
	parent, child := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, child, nil) }()
	if err := parent.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	})
	return parent, done, cancel
}

func mustWrite(t *testing.T, stream io.Writer, frame Frame) {
	t.Helper()
	if err := WriteFrame(stream, frame); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, stream io.Reader, kind string, sequence int) Frame {
	t.Helper()
	frame, err := ReadFrame(stream)
	if err != nil || frame.Kind != kind || frame.Sequence != sequence {
		t.Fatal("帧类型或序号不符", frame.Kind, frame.Sequence, err)
	}
	return frame
}

func TestPrivateStreamConversation(t *testing.T) {
	parent, done, _ := startPeer(t)
	start := fixtureStart()
	mustWrite(t, parent, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
	model := mustRead(t, parent, "model", 1)
	if model.Key != "model.1" || model.Input.System != start.Input.System {
		t.Fatal("模型帧未携带启动上下文")
	}
	output := toolOutput()
	mustWrite(t, parent, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "result", Model: &output})
	tool := mustRead(t, parent, "tool", 2)
	if tool.Key != "tool.1.1" || *tool.Call != output.Tools[0] {
		t.Fatal("工具帧丢失原始参数")
	}
	mustWrite(t, parent, Frame{Version: ProtocolVersion, Sequence: 2, Kind: "result", Tool: &ToolResult{Content: "资料内容"}})
	model = mustRead(t, parent, "model", 3)
	if model.Key != "model.2" || len(model.Input.Messages) != 3 {
		t.Fatal("第二轮模型未携带工具结果")
	}
	arguments, ok := model.Input.Messages[1].Blocks[0].ToolUse.Input.(json.RawMessage)
	if !ok || string(arguments) != output.Tools[0].Arguments {
		t.Fatal("跨进程模型上下文改变工具参数原文或大整数")
	}
	mustWrite(t, parent, Frame{Version: ProtocolVersion, Sequence: 3, Kind: "result", Model: &ModelOutput{Text: "资料已经读取。", StopReason: "stop"}})
	completed := mustRead(t, parent, "done", 4)
	if completed.Answer != "资料已经读取。" || completed.Failure != "" {
		t.Fatal("完成状态不符", completed)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrivateStreamFailsClosed(t *testing.T) {
	t.Run("等待启动内容时取消", func(t *testing.T) {
		_, done, cancel := startPeer(t)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("取消未唤醒启动内容读取")
		}
	})
	for _, test := range []struct {
		name  string
		wrong Frame
	}{
		{"错误序号", Frame{Version: ProtocolVersion, Sequence: 2, Kind: "result", Model: &ModelOutput{Text: "不应接收", StopReason: "stop"}}},
		{"错误结果类型", Frame{Version: ProtocolVersion, Sequence: 1, Kind: "result", Tool: &ToolResult{Content: "不应接收"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, done, _ := startPeer(t)
			start := fixtureStart()
			mustWrite(t, parent, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
			mustRead(t, parent, "model", 1)
			mustWrite(t, parent, test.wrong)
			if _, err := ReadFrame(parent); !errors.Is(err, ErrProtocol) {
				t.Fatal("错误回包后仍发出动作或成功结果", err)
			}
			if err := <-done; !errors.Is(err, ErrProtocol) {
				t.Fatal(err)
			}
		})
	}
	t.Run("等待回包时取消", func(t *testing.T) {
		parent, done, cancel := startPeer(t)
		start := fixtureStart()
		mustWrite(t, parent, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
		mustRead(t, parent, "model", 1)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("取消未唤醒阻塞读取")
		}
	})
	t.Run("无效模型只报告失败", func(t *testing.T) {
		parent, done, _ := startPeer(t)
		start := fixtureStart()
		mustWrite(t, parent, Frame{Version: ProtocolVersion, Kind: "start", Start: &start})
		mustRead(t, parent, "model", 1)
		mustWrite(t, parent, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "result", Model: &ModelOutput{Text: strings.Repeat("x", start.Limits.OutputBytes+1), StopReason: "stop"}})
		completed := mustRead(t, parent, "done", 2)
		if completed.Answer != "" || completed.Failure != "MODEL_OUTPUT_INVALID" {
			t.Fatal(completed)
		}
		if err := <-done; !errors.Is(err, ErrModelOutput) {
			t.Fatal(err)
		}
	})
}

func rawFrame(encoded string) []byte {
	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, uint32(len(encoded)))
	return append(prefix, encoded...)
}

type shortWriter struct{}

func (shortWriter) Write(encoded []byte) (int, error) { return len(encoded) - 1, nil }

func TestFrameRejectsAmbiguousAndUnboundedInput(t *testing.T) {
	for _, encoded := range []string{
		`{"version":1,"sequence":1,"kind":"done"}`,
		`{"version":2,"sequence":1,"kind":"done","answer":"a","answer":"b"}`,
		`{"Version":2,"sequence":1,"kind":"done"}`,
		`{"version":2,"sequence":1,"kind":"done","credential":"not-accepted"}`,
		`{"version":2,"sequence":1,"kind":"done"} {}`,
		`{"version":2,"sequence":1,"kind":"done","failure":"free-form-error"}`,
		`{"version":2,"sequence":1,"kind":"done","input":{}}`,
	} {
		if _, err := ReadFrame(bytes.NewReader(rawFrame(encoded))); !errors.Is(err, ErrProtocol) {
			t.Fatal("接受歧义或越权帧", err)
		}
	}
	for _, size := range []uint32{0, MaximumFrameBytes + 1, ^uint32(0)} {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], size)
		if _, err := ReadFrame(bytes.NewReader(prefix[:])); !errors.Is(err, ErrProtocol) {
			t.Fatal("接受超限长度", size, err)
		}
	}
	if _, err := ReadFrame(bytes.NewReader(rawFrame(`{"version":2`)[:7])); !errors.Is(err, ErrProtocol) {
		t.Fatal("接受截断帧", err)
	}
	if err := WriteFrame(shortWriter{}, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "done", Answer: "完成"}); !errors.Is(err, ErrProtocol) {
		t.Fatal("忽略部分写入", err)
	}
}
