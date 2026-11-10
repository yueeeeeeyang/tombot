package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func TestMemoryToolsRequireApprovalAndBindVersions(t *testing.T) {
	e, bot, run := fixture(t, "standard")
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "memory_write", "memory_search", "memory_delete")
	ctx := context.Background()
	existing, err := e.memories.Write(ctx, bot.ID, "", domain.MemoryInput{Title: "核验偏好", Content: "原有核验偏好", Importance: 3}, "", "user")
	if err != nil {
		t.Fatal(err)
	}
	result := execute(t, e, bot, run, "memory_delete", map[string]any{"path": existing.Path})
	if result.IsError || !strings.Contains(result.Content, `"deleted":true`) {
		t.Fatal(result)
	}
	if _, err = e.memories.Read(ctx, bot.ID, existing.Path); err == nil {
		t.Fatal("删除没有立即生效")
	}
}

func TestMemoryToolsWriteSearchIsolationAndFileHook(t *testing.T) {
	e, bot, run := fixture(t, "loose")
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "memory_write", "memory_search", "memory_delete")
	result := execute(t, e, bot, run, "memory_write", map[string]any{"title": "文风偏好", "summary": "使用简洁中文。", "content": "避免冗长开头。", "tags": []string{"writing"}, "importance": 4})
	if result.IsError {
		t.Fatal(result)
	}
	var entry domain.Memory
	if err := json.Unmarshal([]byte(result.Content), &entry); err != nil {
		t.Fatal(err)
	}
	result = execute(t, e, bot, run, "memory_search", map[string]any{"query": "简洁中文"})
	if result.IsError || !strings.Contains(result.Content, entry.Path) {
		t.Fatal(result)
	}
	result = execute(t, e, bot, run, "memory_search", map[string]any{"query": "偏好", "bot_id": "another"})
	if !result.IsError || !strings.Contains(result.Content, "INVALID_TOOL_INPUT") {
		t.Fatal("接受了跨 Bot 参数", result)
	}
	result = execute(t, e, bot, run, "memory_delete", map[string]any{"path": "shared/record.md"})
	if !result.IsError {
		t.Fatal("允许删除非私有记忆", result)
	}
	result = execute(t, e, bot, run, "file_edit", map[string]any{"path": entry.Path, "old_text": "简洁中文", "new_text": "清晰表达"})
	if result.IsError {
		t.Fatal(result)
	}
	var count int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM memory_fts_cjk WHERE memory_fts_cjk MATCH '"清晰表达"' AND bot_id=?`, bot.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("文件工具未同步索引", count, err)
	}
	result = execute(t, e, bot, run, "memory_write", map[string]any{"title": entry.Title, "content": "覆盖旧记忆", "path": entry.Path, "expected_sha256": entry.SHA256})
	if !result.IsError || !strings.Contains(result.Content, "MEMORY_CHANGED") {
		t.Fatal("过期写入没有冲突", result)
	}
}
