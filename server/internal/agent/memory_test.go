package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func TestRuntimeInjectsOnlyOwnBoundedMemoryIndex(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if !strings.Contains(req.System, "私有工作偏好") || strings.Contains(req.System, "其他同事的私有经验") {
			return provider.Usage{}, errors.New("记忆提示未正确隔离")
		}
		index := strings.Index(req.System, "\n\nPrivate memory index")
		if index < 0 || len(req.System[index:]) > domain.MaxMemoryIndexBytes {
			return provider.Usage{}, errors.New("注入的记忆索引超限")
		}
		encoded, err := json.Marshal(req.Messages)
		if err != nil {
			return provider.Usage{}, err
		}
		if strings.Contains(string(encoded), "私有工作偏好") {
			return provider.Usage{}, errors.New("记忆索引被复制成会话转录")
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "遵循已保存偏好"})
	}), DefaultLimits())
	for index := 0; index < 8; index++ {
		_, err := r.deps.Memories.Write(context.Background(), bot.ID, ".tom/memory/"+strings.Repeat("x", index+1)+".md", domain.MemoryInput{Title: "私有工作偏好", Content: strings.Repeat("需要校验的描述", 30), Importance: 3}, "", "user")
		if err != nil {
			t.Fatal(err)
		}
	}
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.deps.Memories.Write(context.Background(), peer.ID, "", domain.MemoryInput{Title: "其他同事的私有经验", Content: "不应出现在调用方提示。", Importance: 5}, "", "user"); err != nil {
		t.Fatal(err)
	}
	sent := submit(t, r, bot)
	if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
}
