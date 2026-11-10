package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

func introductionMemory(t *testing.T, r *Runtime, botID string, previous domain.Memory, summary string) domain.Memory {
	t.Helper()
	memory, err := r.deps.Memories.Write(context.Background(), botID, previous.Path, domain.MemoryInput{Title: "协作习惯", Summary: summary, Content: "只应留在记忆正文的细节", Importance: 3}, previous.SHA256, "user")
	if err != nil {
		t.Fatal(err)
	}
	return memory
}

func TestIntroductionCacheInvalidationAndWorkIsolation(t *testing.T) {
	var calls atomic.Int32
	const introduction = "熟悉你的创作节奏，喜欢陪你把灵感整理成故事。"
	r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.HasPrefix(req.System, "Write a short Chinese introduction") {
			calls.Add(1)
			if len(req.Tools) != 0 || len(req.Messages) != 1 || req.Model.MaxOutput != introductionOutputTokens {
				return provider.Usage{}, errors.New("介绍请求不应携带工具或聊天记录")
			}
			input := req.Messages[0].Blocks[0].Text
			if !strings.Contains(input, "协作习惯") || strings.Contains(input, "只应留在记忆正文的细节") || strings.Contains(input, ".tom/") {
				return provider.Usage{}, errors.New("介绍未只使用记忆摘要")
			}
			return provider.Usage{InputTokens: 20, OutputTokens: 10}, emit(provider.Event{Kind: "text_delta", Text: introduction})
		}
		if strings.Contains(req.System, introduction) {
			return provider.Usage{}, errors.New("介绍反向进入工作提示")
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "完成"})
	}), DefaultLimits())
	ctx := context.Background()
	empty, err := r.Introduction(ctx, bot.ID)
	if err != nil || empty.Text != "" || calls.Load() != 0 {
		t.Fatal(empty, err, calls.Load())
	}
	memory := introductionMemory(t, r, bot.ID, domain.Memory{}, "习惯先整理创作提纲")
	for range 2 {
		result, err := r.Introduction(ctx, bot.ID)
		if err != nil || result.Text != introduction {
			t.Fatal(result, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("缓存未命中", calls.Load())
	}
	memory = introductionMemory(t, r, bot.ID, memory, "现在习惯先讨论再写提纲")
	if _, err := r.Introduction(ctx, bot.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("记忆变化未重新总结", calls.Load())
	}
	var messages, cost int
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", bot.ConversationID).Scan(&messages); err != nil || messages != 0 {
		t.Fatal("介绍写入了聊天", messages, err)
	}
	if err := r.deps.DB.QueryRow("SELECT cost_micro FROM bot_introductions WHERE bot_id=?", bot.ID).Scan(&cost); err != nil || cost != 60 {
		t.Fatal("介绍用量丢失", cost, err)
	}
	saved, err := r.deps.Bots.Get(ctx, bot.ID)
	if err != nil || saved.Soul != bot.Soul || saved.RoleTitle != bot.RoleTitle {
		t.Fatal("介绍修改了身份", saved, err)
	}
	sent := submit(t, r, bot)
	if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
	if err := r.deps.Memories.Delete(ctx, bot.ID, memory.Path, memory.SHA256, "user"); err != nil {
		t.Fatal(err)
	}
	result, err := r.Introduction(ctx, bot.ID)
	if err != nil || result.Text != "" || calls.Load() != 2 {
		t.Fatal("遗忘后仍保留旧介绍", result, err)
	}
}

func TestIntroductionRejectsInvalidOutputAndKeepsUsage(t *testing.T) {
	for _, test := range []struct {
		name, text, kind string
		failure          error
	}{
		{name: "空输出", kind: "text_delta"},
		{name: "超长", kind: "text_delta", text: strings.Repeat("猫", 121)},
		{name: "工具调用", kind: "tool_call"},
		{name: "上游失败", failure: errors.New("模型不可用")},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, bot := runtimeFixture(t, script(func(_ context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				err := test.failure
				if err == nil {
					err = emit(provider.Event{Kind: test.kind, Text: test.text})
				}
				return provider.Usage{InputTokens: 2}, err
			}), DefaultLimits())
			introductionMemory(t, r, bot.ID, domain.Memory{}, "喜欢简短反馈")
			if _, err := r.Introduction(context.Background(), bot.ID); err == nil {
				t.Fatal("无效介绍被接受")
			}
			var text, digest string
			var tokens int
			if err := r.deps.DB.QueryRow("SELECT text,source_digest,input_tokens FROM bot_introductions WHERE bot_id=?", bot.ID).Scan(&text, &digest, &tokens); err != nil || text != "" || digest != "" || tokens != 2 {
				t.Fatal("失败被缓存为成功或丢失用量", text, digest, tokens, err)
			}
		})
	}
}

func TestIntroductionDiscardsResultWhenMemoriesChangeDuringGeneration(t *testing.T) {
	var update func()
	r, bot := runtimeFixture(t, script(func(_ context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		update()
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "旧介绍"})
	}), DefaultLimits())
	memory := introductionMemory(t, r, bot.ID, domain.Memory{}, "旧习惯")
	update = func() { introductionMemory(t, r, bot.ID, memory, "新习惯") }
	_, err := r.Introduction(context.Background(), bot.ID)
	requireCode(t, err, "INTRODUCTION_CHANGED")
	var text string
	if err := r.deps.DB.QueryRow("SELECT text FROM bot_introductions WHERE bot_id=?", bot.ID).Scan(&text); err != nil || text != "" {
		t.Fatal("写入了过时介绍", text, err)
	}
}

func TestIntroductionCoalescesRequestsAndClosesInFlightWork(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	limits := DefaultLimits()
	limits.Concurrent = 1
	r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return provider.Usage{}, ctx.Err()
	}), limits)
	introductionMemory(t, r, bot.ID, domain.Memory{}, "协作")
	completed := make(chan error, 1)
	go func() { _, err := r.Introduction(context.Background(), bot.ID); completed <- err }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Introduction(ctx, bot.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("等待者无法取消", err)
	}
	_, err := r.Introduction(context.Background(), "other")
	requireCode(t, err, "INTRODUCTION_BUSY")
	r.Close()
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatal("关闭未取消在途生成", err)
	}
	if calls.Load() != 1 {
		t.Fatal("同猫请求未合并", calls.Load())
	}
	_, err = r.Introduction(context.Background(), bot.ID)
	requireCode(t, err, "RUNTIME_CLOSED")
}
