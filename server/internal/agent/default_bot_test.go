package agent

import (
	"context"
	"strings"
	"testing"

	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func TestDefaultBotRunsThroughNormalModelAdmission(t *testing.T) {
	runtime, _ := runtimeFixture(t, script(func(ctx context.Context, request provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if !strings.Contains(request.System, "You are Tom, a general-purpose assistant.") {
			t.Error("默认助手身份没有进入正常执行上下文")
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "你好，我是 Tom。"})
	}), DefaultLimits())
	if err := runtime.deps.Bots.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	bot, err := runtime.deps.Bots.Get(context.Background(), service.DefaultBotID)
	if err != nil || bot.ModelID == nil {
		t.Fatal("默认助手没有绑定已有默认模型", err)
	}
	sent := submit(t, runtime, bot)
	run := waitRun(t, runtime, sent.RunID)
	if run.Status != "succeeded" || run.BotID != service.DefaultBotID || run.ModelID != *bot.ModelID {
		t.Fatal("默认助手未通过现有模型和事务校验", run.Status)
	}
}
