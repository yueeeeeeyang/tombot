package botagent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/modelgateway"
)

func TestDispatchAuditsContainerConversation(t *testing.T) {
	parent, child := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, child, nil) }()
	models, tools := 0, 0
	relay := testRelay{model: func(_ context.Context, key string, _ modelgateway.Input) (ModelOutput, error) {
		models++
		if key == "model.1" {
			return toolOutput(), nil
		}
		return ModelOutput{Text: "完成", StopReason: "stop"}, nil
	}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
		tools++
		return ToolResult{Content: "文件内容"}, nil
	}}
	answer, err := DispatchStream(ctx, parent, fixtureStart(), relay)
	if err != nil || answer != "完成" || models != 2 || tools != 1 {
		t.Fatal(answer, models, tools, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDispatchRejectsUntrustedActions(t *testing.T) {
	for _, name := range []string{"步骤跳跃", "提示词覆盖", "工具定义覆盖", "绕过模型执行工具", "伪造完成"} {
		t.Run(name, func(t *testing.T) {
			parent, child := net.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				first, err := ReadFrame(child)
				if err != nil {
					done <- errors.Join(err, child.Close())
					return
				}
				frame := Frame{Version: ProtocolVersion, Sequence: 1, Kind: "model", Key: "model.1", Input: &first.Start.Input}
				switch name {
				case "步骤跳跃":
					frame.Key = "model.2"
				case "提示词覆盖":
					frame.Input.System = "Unapproved system prompt."
				case "工具定义覆盖":
					frame.Input.Tools[0].Description = "Unapproved tool definition."
				case "绕过模型执行工具":
					frame = Frame{Version: ProtocolVersion, Sequence: 1, Kind: "tool", Key: "tool.1.1", Call: &toolOutput().Tools[0]}
				case "伪造完成":
					frame = Frame{Version: ProtocolVersion, Sequence: 1, Kind: "done", Answer: "伪造回复"}
				}
				err = WriteFrame(child, frame)
				done <- errors.Join(err, child.Close())
			}()
			calls := 0
			relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
				calls++
				return ModelOutput{}, nil
			}, tool: func(context.Context, string, ToolCall) (ToolResult, error) { calls++; return ToolResult{}, nil }}
			if _, err := DispatchStream(ctx, parent, fixtureStart(), relay); !errors.Is(err, ErrProtocol) || calls != 0 {
				t.Fatal("未经授权的动作到达中继", calls, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDispatchBoundariesAndModelFailure(t *testing.T) {
	for _, kind := range []string{"轮次上限", "上下文上限", "模型调用未知"} {
		t.Run(kind, func(t *testing.T) {
			parent, child := net.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, child, nil) }()
			start := fixtureStart()
			want := ErrOperationLimit
			if kind == "轮次上限" {
				start.Limits.ModelSteps = 1
			}
			if kind == "上下文上限" {
				start.Limits.ContextBytes = 1024
				want = ErrContextLimit
			}
			unknown := errors.New("模型调用结果未知")
			if kind == "模型调用未知" {
				want = unknown
			}
			models, tools := 0, 0
			relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
				models++
				if kind == "模型调用未知" {
					return ModelOutput{}, unknown
				}
				return toolOutput(), nil
			}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
				tools++
				return ToolResult{Content: strings.Repeat("x", 900)}, nil
			}}
			if _, err := DispatchStream(ctx, parent, start, relay); !errors.Is(err, want) || models != 1 || kind == "模型调用未知" && tools != 0 {
				t.Fatal(models, tools, err)
			}
			if err := <-done; err == nil {
				t.Fatal("失败运行报告 Agent 成功")
			}
		})
	}
}
