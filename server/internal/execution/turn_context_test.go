package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/botpersona"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestCurrentTurnKeepsCompletedWorkAsHistory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, pgtest.Start(t))
	bot := f.plainBot(t)
	const task = "把今天的核对结论发到聊天。"
	const result = "核对完成，记录一致。"
	var current string
	worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		persona := botpersona.ForCurrentTurn(botpersona.System(bot.Name, bot.Description, bot.AvatarRef, bot.SystemPrompt))
		if !strings.HasPrefix(input.System, persona) {
			t.Error("发送给模型的请求缺少本轮边界")
		}
		last := input.Messages[len(input.Messages)-1]
		if receipt := last.Blocks[0].ToolResult; receipt != nil {
			if receipt.IsError {
				t.Fatal("主动投递失败", receipt)
			}
			return emitText(emit, "结论已发到聊天。")
		}
		if last.Role != "user" || last.Blocks[0].Text != current {
			t.Fatalf("本轮输入丢失：%+v", last)
		}
		if current != task {
			if len(input.Messages) != 2 || len(input.Messages[0].Blocks) != 1 {
				t.Fatal("旧消息作为独立对话消息进入了本轮")
			}
			archive, ok := strings.CutPrefix(input.Messages[0].Blocks[0].Text, historyContextLabel)
			var history [][]string
			if !ok || json.Unmarshal([]byte(archive), &history) != nil || len(history) < 3 || len(history[0]) < 2 || history[0][1] != task || len(history[1]) < 2 || history[1][1] != result {
				t.Fatal("旧请求和已投递结果没有一起保留在历史记录中")
			}
		}
		switch current {
		case "你好":
			return emitText(emit, "你好！")
		case "上次核对结果是什么？":
			return emitText(emit, result)
		default:
			return emitMemory(emit, "send_user_message", memoryArguments(t, botmessaging.UserMessage{Message: result}))
		}
	}))
	for _, scenario := range []struct {
		request    string
		deliveries int
	}{
		{task, 1},
		{"你好", 1},
		{"上次核对结果是什么？", 1},
		{"请按上次方案重做并再发一次。", 2},
	} {
		current = scenario.request
		if _, err := f.service.Send(ctx, f.a, bot.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: current}); err != nil {
			t.Fatal(err)
		}
		if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" {
			t.Fatal("本轮回复失败", current, run.Status, run.ErrorCode, err)
		}
		var deliveries int
		err := f.pool.QueryRow(`SELECT count(*) FROM tombot.turns WHERE user_id=$1 AND conversation_id=$2 AND source='proactive'`, f.a.UserID, bot.ConversationID).Scan(&deliveries)
		if err != nil || deliveries != scenario.deliveries {
			t.Fatalf("%q 后期望累计投递 %d 次，实际 %d 次：%v", current, scenario.deliveries, deliveries, err)
		}
	}
}

func TestScopeModelTurnPreservesActiveToolSequence(t *testing.T) {
	old := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "派发旧任务"}}}
	current := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "核对新任务"}}}
	call := provider.Message{Role: "assistant", Blocks: []domain.MessageBlock{{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "call", Name: "file_read", Input: json.RawMessage(`{}`)}}}}
	result := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "call"}}}}
	steering := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "只检查第二份文件"}}}
	input := modelgateway.Input{Messages: []provider.Message{old, current, old, call, result, steering}}
	output, err := scopeModelTurn(input, []int{0, 2}, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Messages) != 5 || output.Messages[1].Blocks[0].Text != current.Blocks[0].Text || output.Messages[2].Blocks[0].ToolUse != call.Blocks[0].ToolUse || output.Messages[3].Blocks[0].ToolResult != result.Blocks[0].ToolResult || output.Messages[4].Blocks[0].Text != steering.Blocks[0].Text {
		t.Fatal("历史归档改变了当前消息、工具调用和新增输入的顺序")
	}
	if input.System != "" || len(input.Messages) != 6 || input.Messages[0].Blocks[0].Text != old.Blocks[0].Text {
		t.Fatal("历史归档改写了原始恢复轨迹")
	}
	if _, err := scopeModelTurn(input, []int{0, 2}, 100); err != ErrContextLimit {
		t.Fatal("模型上下文预算不足时仍允许截断本轮输入", err)
	}
}

func TestScopeModelTurnTrimsOnlyOldestHistoryToFit(t *testing.T) {
	oldest := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "最早的要求" + strings.Repeat(`"\`, 250)}}}
	recent := provider.Message{Role: "assistant", Blocks: []domain.MessageBlock{{Kind: "text", Text: "已交付的结果"}}}
	current := provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "你好"}}}
	input := modelgateway.Input{Messages: []provider.Message{oldest, recent, current}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	output, err := scopeModelTurn(input, []int{0, 1}, len(encoded))
	if err != nil || len(output.Messages) != 2 || output.Messages[1].Blocks[0].Text != "你好" {
		t.Fatal("历史封装超限导致本轮消息丢失", err)
	}
	archive := output.Messages[0].Blocks[0].Text
	if !strings.Contains(archive, "已交付的结果") || strings.Contains(archive, "最早的要求") {
		t.Fatal("预算不足时应只省略最早的历史")
	}
}
