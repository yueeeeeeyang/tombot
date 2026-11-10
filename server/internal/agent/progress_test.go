package agent

import (
	"context"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

func TestProgressValidationAndEventScope(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	}), DefaultLimits())
	j := &job{run: domain.Run{ID: "run-progress", TurnID: "turn-progress", ConversationID: bot.ConversationID, BotID: bot.ID}}
	for _, input := range []any{nil, map[string]any{}, map[string]any{"summary": " "}, map[string]any{"summary": strings.Repeat("字", 61)}, map[string]any{"summary": "两行\n摘要"}, map[string]any{"summary": 1}, map[string]any{"summary": "摘要", "extra": true}} {
		result, err := r.reportProgress(context.Background(), j, domain.ToolUseBlock{ID: "call", Input: input})
		if err != nil || !result.IsError || result.ToolCallID != "call" {
			t.Fatalf("非法摘要未拒绝：%+v %v", result, err)
		}
	}
	result, err := r.reportProgress(context.Background(), j, domain.ToolUseBlock{ID: "valid", Input: map[string]any{"summary": "正在核对任务清单"}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var count int
	if err = r.deps.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='run.progress' AND conversation_id=? AND json_extract(payload_json,'$.turn_id')='turn-progress' AND json_extract(payload_json,'$.text')='正在核对任务清单'`, bot.ConversationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("进度未绑定回合或非法事件被保存", count, err)
	}
}

func TestWelcomeDoesNotImpersonateUserOrRepeat(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(_ context.Context, request provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if !strings.Contains(request.Messages[0].Blocks[0].Text, "Greet them in Chinese") {
			t.Error("缺少欢迎指令")
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "你好，我们一起开始吧。"})
	}), DefaultLimits())
	sent, err := r.Welcome(context.Background(), bot.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
	var sender string
	if err := r.deps.DB.QueryRow("SELECT sender_kind FROM messages WHERE id=?", sent.MessageID).Scan(&sender); err != nil || sender != "system" {
		t.Fatal("欢迎不能冒充用户", sender, err)
	}
	if _, err = r.Welcome(context.Background(), bot.ConversationID); err == nil {
		t.Fatal("不应重复欢迎")
	}
	var count int
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM turns WHERE conversation_id=?", bot.ConversationID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestWelcomeFailureAndExistingConversation(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, domain.NewError(502, "MODEL_FAILED", "The model failed.")
	}), DefaultLimits())
	sent, err := r.Welcome(context.Background(), bot.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if run := waitRun(t, r, sent.RunID); run.Status != "failed" {
		t.Fatal(run)
	}
	if _, err = r.Welcome(context.Background(), bot.ConversationID); err == nil {
		t.Fatal("失败回合也不能被自动重复提交")
	}
}
