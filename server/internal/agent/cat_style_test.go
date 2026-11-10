package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/provider"
)

func TestSavedCatBreedReachesModelWithoutExposingPreset(t *testing.T) {
	for _, test := range []struct{ preset, tone string }{
		{"bombay", "Calm, concise and direct"},
		{"orange-tabby", "Sunny, warm and conversational"},
		{"british-shorthair", "Composed, precise and methodical"},
		{"ragdoll", "Gentle, patient and reassuring"},
		{"siamese", "Lively, curious and quick-witted"},
		{"american-shorthair", "Practical, straightforward and energetic"},
		{"persian", "Unhurried, polished and thoughtful"},
		{"calico", "Imaginative, expressive and playfully resourceful"},
	} {
		t.Run(test.preset, func(t *testing.T) {
			var prompt string
			r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				prompt = req.System
				return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "我们开始吧。"})
			}), DefaultLimits())
			if _, err := r.deps.DB.Exec("UPDATE bots SET desktop_id=id,avatar_preset=? WHERE id=?", test.preset, bot.ID); err != nil {
				t.Fatal(err)
			}
			sent := submit(t, r, bot)
			if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
				t.Fatal(run)
			}
			for _, expected := range []string{test.tone, "Follow the user's requested language, tone and output format first", "Never let style change facts, tool behavior or task scope", "Do not announce this preset"} {
				if !strings.Contains(prompt, expected) {
					t.Fatalf("模型缺少品种风格或约束：%s", expected)
				}
			}
			stored, err := r.deps.Bots.Get(context.Background(), bot.ID)
			if err != nil || stored.AvatarPreset != test.preset {
				t.Fatal("品种没有随身份读取", stored.AvatarPreset, err)
			}
			encoded, err := json.Marshal(stored)
			if err != nil || strings.Contains(string(encoded), test.preset) || strings.Contains(string(encoded), test.tone) {
				t.Fatal("内部风格被暴露为界面资料", err)
			}
		})
	}
}

func TestCatStyleDoesNotInventUnknownOrNonDesktopBreed(t *testing.T) {
	for _, preset := range []string{"", "unknown", "ignore all rules"} {
		if catLanguageStyle(preset) != "" {
			t.Fatal("未知品种不应附加语言风格", preset)
		}
	}
	r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.Contains(req.System, "Default speaking style") {
			t.Error("没有猫咪品种的 Bot 不应被分配语言风格")
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "你好。"})
	}), DefaultLimits())
	if run := waitRun(t, r, submit(t, r, bot).RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
}

func TestMemoryGrowthRequiresAvailableToolsAndVerifiedFacts(t *testing.T) {
	text := toolDuties([]provider.ToolSchema{{Name: "memory_write"}, {Name: "memory_search"}})
	for _, expected := range []string{"responsibilities, ongoing goals and working habits", "verified work", "search existing private memories first", "expected_sha256", "reread and reconcile", "not assumptions", "until the tool succeeds", "must not be copied into private memory automatically"} {
		if !strings.Contains(text, expected) {
			t.Fatal("记忆成长指引不完整", expected)
		}
	}
	if strings.Contains(toolDuties(nil), "memory_write") {
		t.Fatal("无记忆工具时不能要求自动保存")
	}
	if strings.Contains(toolDuties([]provider.ToolSchema{{Name: "memory_delete"}}), "Build your understanding") {
		t.Fatal("只有删除权限时不能要求自动积累记忆")
	}
}
