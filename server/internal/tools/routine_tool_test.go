package tools

import (
	"context"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

func TestLooseBotWritesMemoryAndRoutineWithoutApproval(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"memory_write", "memory_delete", "routine_list", "routine_upsert", "routine_delete"}
	bot.ToolPolicy.Approval = "loose"
	names := map[string]bool{}
	for _, schema := range e.Schemas(bot) {
		names[schema.Name] = true
	}
	for _, name := range bot.ToolPolicy.AllowedTools {
		if !names[name] {
			t.Fatalf("工具列表缺少 %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	memoryResult, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "memory_write", Input: map[string]any{"title": "晨跑", "content": "用户每周三晨跑"}})
	if err != nil || memoryResult.IsError {
		t.Fatalf("记忆没有写入: %v %s", err, memoryResult.Content)
	}
	routineResult, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "routine_upsert", Input: map[string]any{"name": "晨跑提醒", "cron": "0 9 * * 3", "prompt": "提醒用户周三晨跑"}})
	if err != nil || routineResult.IsError {
		t.Fatalf("定时任务没有创建: %v %s", err, routineResult.Content)
	}
	var approvals int
	if err = e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals").Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("宽松策略仍创建了 %d 条审批", approvals)
	}
	memories, err := e.memories.List(ctx, bot.ID)
	if err != nil || len(memories) != 1 || memories[0].Title != "晨跑" {
		t.Fatalf("记忆列表不对: %+v %v", memories, err)
	}
	routines, err := e.routines.List(ctx, bot.ID, "", "", 20)
	if err != nil || len(routines) != 1 || routines[0].Name != "晨跑提醒" || !routines[0].IsEnabled {
		t.Fatalf("例程列表不对: %+v %v", routines, err)
	}
}
