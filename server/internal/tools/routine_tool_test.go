package tools

import (
	"context"
	"strings"
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
	memories, err := e.memories.List(ctx, bot.ID)
	if err != nil || len(memories) != 1 || memories[0].Title != "晨跑" {
		t.Fatalf("记忆列表不对: %+v %v", memories, err)
	}
	routines, err := e.routines.List(ctx, bot.ID, "", "", 20)
	if err != nil || len(routines) != 1 || routines[0].Name != "晨跑提醒" || !routines[0].IsEnabled {
		t.Fatalf("例程列表不对: %+v %v", routines, err)
	}
}

func TestWorkingHabitsPersistAndRejectStaleUpdates(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = []string{"memory_search", "memory_write"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	write := func(input map[string]any) domain.ToolResultBlock {
		t.Helper()
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "memory_write", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := write(map[string]any{"title": "协作习惯", "summary": "负责整理周报，先给结论", "content": "根据用户安排负责周报整理；汇报时先给结论，再列依据。"})
	if first.IsError {
		t.Fatal(first.Content)
	}
	entries, err := e.memories.Search(ctx, bot.ID, "协作习惯", 5)
	if err != nil || len(entries) != 1 {
		t.Fatal("已形成的习惯必须可检索", entries, err)
	}
	entry := entries[0]
	updated := write(map[string]any{"path": entry.Path, "expected_sha256": entry.SHA256, "title": "协作习惯", "summary": "负责整理周报，先给结论，再列风险", "content": "用户补充：先给结论，再列风险，最后说明依据。"})
	if updated.IsError {
		t.Fatal(updated.Content)
	}
	stale := write(map[string]any{"path": entry.Path, "expected_sha256": entry.SHA256, "title": "协作习惯", "content": "过期的旧习惯"})
	if !stale.IsError {
		t.Fatal("旧版本不能覆盖最新习惯")
	}
	stored, err := e.files.ReadMemory(ctx, bot.ID, entry.Path)
	if err != nil || !strings.Contains(stored.Content, "再列风险") || strings.Contains(stored.Content, "过期的旧习惯") {
		t.Fatal("习惯更新未持久化或被过期写入覆盖", err)
	}
	index, err := e.memories.PromptIndex(ctx, bot.ID)
	if err != nil || !strings.Contains(index, "负责整理周报，先给结论，再列风险") {
		t.Fatal("后续对话不能读取新的职责与习惯", err)
	}
	entries, err = e.memories.List(ctx, bot.ID)
	if err != nil || len(entries) != 1 {
		t.Fatal("更新不应累积重复笔记", len(entries), err)
	}
}
