package botagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

type testRelay struct {
	model func(context.Context, string, modelgateway.Input) (ModelOutput, error)
	tool  func(context.Context, string, ToolCall) (ToolResult, error)
}

func (relay testRelay) Model(ctx context.Context, key string, input modelgateway.Input) (ModelOutput, error) {
	return relay.model(ctx, key, input)
}
func (relay testRelay) Tool(ctx context.Context, key string, call ToolCall) (ToolResult, error) {
	return relay.tool(ctx, key, call)
}

func fixtureStart() Start {
	return Start{Input: modelgateway.Input{System: "Complete the user's task.", Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "读取共享文件并汇总"}}}}, Tools: []provider.ToolSchema{{Name: "file_read", Description: "Read a shared file.", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, Limits: Limits{ModelSteps: 4, ToolCalls: 4, OutputBytes: 4096, ContextBytes: 16 << 10}}
}

func toolOutput() ModelOutput {
	return ModelOutput{Tools: []ToolCall{{ID: "call-one", Name: "file_read", Arguments: `{"version":9007199254740993,"path":"/workspace/共享文件"}`}}, StopReason: "tool_calls"}
}

func TestAgentConversation(t *testing.T) {
	start := fixtureStart()
	before, _ := json.Marshal(start)
	keys := []string{}
	relay := testRelay{
		model: func(_ context.Context, key string, input modelgateway.Input) (ModelOutput, error) {
			keys = append(keys, key)
			if key == "model.1" {
				return toolOutput(), nil
			}
			if len(input.Messages) != 3 || input.Messages[1].Role != "assistant" || input.Messages[2].Role != "user" {
				t.Fatal("循环没有追加对应消息", input.Messages)
			}
			use := input.Messages[1].Blocks[0].ToolUse
			if use == nil || string(use.Input.(json.RawMessage)) != toolOutput().Tools[0].Arguments {
				t.Fatal("工具参数原文或大整数改变")
			}
			result := input.Messages[2].Blocks[0].ToolResult
			if result == nil || result.ToolCallID != "call-one" || result.Content != "文件不存在" || !result.IsError {
				t.Fatal("已知工具拒绝未交给模型")
			}
			return ModelOutput{Text: "未找到指定文件。", StopReason: "end_turn"}, nil
		},
		tool: func(_ context.Context, key string, call ToolCall) (ToolResult, error) {
			keys = append(keys, key)
			if call != toolOutput().Tools[0] {
				t.Fatal("工具调用内容改变")
			}
			return ToolResult{Content: "文件不存在", IsError: true}, nil
		},
	}
	answer, err := Run(context.Background(), start, relay)
	if err != nil || answer != "未找到指定文件。" || !reflect.DeepEqual(keys, []string{"model.1", "tool.1.1", "model.2"}) {
		t.Fatal(answer, keys, err)
	}
	after, _ := json.Marshal(start)
	if string(before) != string(after) {
		t.Fatal("启动快照被执行循环改写")
	}
}

func TestAgentRejectsInvalidModelBeforeTool(t *testing.T) {
	for _, invalid := range []struct {
		name   string
		change func(*ModelOutput)
	}{
		{"未知工具", func(output *ModelOutput) { output.Tools[0].Name = "ungranted_tool" }},
		{"重复调用标识", func(output *ModelOutput) { output.Tools = append(output.Tools, output.Tools[0]) }},
		{"非对象参数", func(output *ModelOutput) { output.Tools[0].Arguments = `[]` }},
		{"不完整模型输出", func(output *ModelOutput) { output.StopReason = "max_tokens" }},
		{"缺失停止标记", func(output *ModelOutput) { output.StopReason = "" }},
		{"超限输出", func(output *ModelOutput) { output.Text = strings.Repeat("x", 4097) }},
		{"控制字符", func(output *ModelOutput) { output.Text = "x\x00" }},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			output := toolOutput()
			invalid.change(&output)
			relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) { return output, nil }, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
				t.Fatal("无效输出导致外部工具执行")
				return ToolResult{}, nil
			}}
			if _, err := Run(context.Background(), fixtureStart(), relay); !errors.Is(err, ErrModelOutput) {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentBoundariesAndNoReplay(t *testing.T) {
	t.Run("初始上下文超限不调用模型", func(t *testing.T) {
		start := fixtureStart()
		start.Limits.ContextBytes = 1024
		start.Input.System = strings.Repeat("x", 1024)
		if _, err := Run(context.Background(), start, testRelay{}); !errors.Is(err, ErrContextLimit) {
			t.Fatal(err)
		}
	})
	t.Run("工具后上下文超限停止", func(t *testing.T) {
		start := fixtureStart()
		start.Limits.ContextBytes = 1024
		calls := 0
		relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
			calls++
			return toolOutput(), nil
		}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
			return ToolResult{Content: strings.Repeat("x", 900)}, nil
		}}
		if _, err := Run(context.Background(), start, relay); !errors.Is(err, ErrContextLimit) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
	t.Run("轮次达到上限", func(t *testing.T) {
		start := fixtureStart()
		start.Limits.ModelSteps = 1
		calls := 0
		relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
			calls++
			return toolOutput(), nil
		}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
			return ToolResult{Content: "已读取"}, nil
		}}
		if _, err := Run(context.Background(), start, relay); !errors.Is(err, ErrOperationLimit) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
	t.Run("未知工具结果不重试", func(t *testing.T) {
		calls := 0
		unknown := errors.New("外部结果未知")
		relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) { return toolOutput(), nil }, tool: func(context.Context, string, ToolCall) (ToolResult, error) { calls++; return ToolResult{}, unknown }}
		if _, err := Run(context.Background(), fixtureStart(), relay); !errors.Is(err, unknown) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
	t.Run("模型返回时取消禁止工具动作", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
			cancel()
			return toolOutput(), nil
		}}
		if _, err := Run(ctx, fixtureStart(), relay); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("两个工具之间取消停止剩余动作", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
			output := toolOutput()
			next := output.Tools[0]
			next.ID = "call-two"
			output.Tools = append(output.Tools, next)
			return output, nil
		}, tool: func(context.Context, string, ToolCall) (ToolResult, error) {
			calls++
			cancel()
			return ToolResult{}, nil
		}}
		if _, err := Run(ctx, fixtureStart(), relay); !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal(calls, err)
		}
	})
}
