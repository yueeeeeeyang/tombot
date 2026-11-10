package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

type listingTools struct{}

func (listingTools) Schemas(bot domain.Bot) []provider.ToolSchema {
	schemas := make([]provider.ToolSchema, 0, len(bot.ToolPolicy.AllowedTools))
	for _, name := range bot.ToolPolicy.AllowedTools {
		schemas = append(schemas, provider.ToolSchema{Name: name, Description: name})
	}
	return schemas
}

func (listingTools) Execute(context.Context, domain.ToolInvocation, domain.ToolUseBlock) (domain.ToolResultBlock, error) {
	return domain.ToolResultBlock{}, nil
}

func TestSystemPromptAndToolListExposeMemoryAndRoutines(t *testing.T) {
	var prompt string
	var tools []string
	r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		prompt = req.System
		for _, tool := range req.Tools {
			tools = append(tools, tool.Name)
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "好"})
	}), DefaultLimits())
	r.deps.Tools = listingTools{}
	policy := domain.ToolPolicy{AllowedTools: []string{"memory_search", "memory_write", "memory_delete", "routine_list", "routine_upsert", "routine_delete"}, Approval: "loose"}
	if _, err := r.deps.Bots.Update(context.Background(), bot.ID, service.BotPatch{ToolPolicy: service.Field[domain.ToolPolicy]{Set: true, Value: policy}}, "user"); err != nil {
		t.Fatal(err)
	}
	sent := submit(t, r, bot)
	if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
	for _, name := range policy.AllowedTools {
		if !slices.Contains(tools, name) {
			t.Fatalf("工具列表缺少 %s：%v", name, tools)
		}
	}
	if !strings.Contains(prompt, "call memory_write, memory_search, or memory_delete") || !strings.Contains(prompt, "call routine_list, routine_upsert, or routine_delete") {
		t.Fatalf("系统提示没有要求使用记忆和例程工具：%s", prompt)
	}
}
