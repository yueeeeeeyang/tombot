package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
)

func TestMemoryToolsRequireApprovalAndBindVersions(t *testing.T) {
	for _, decision := range []string{"approve", "deny", "changed"} {
		t.Run(decision, func(t *testing.T) {
			e, bot, run := fixture(t, "standard")
			bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "memory_write", "memory_search", "memory_delete")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			existing, err := e.memories.Write(ctx, bot.ID, "", domain.MemoryInput{Title: "核验偏好", Content: "原有核验偏好", Importance: 3}, "", "user")
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan domain.ToolResultBlock, 1)
			failures := make(chan error, 1)
			go func() {
				result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot}, domain.ToolUseBlock{ID: ids.New(), Name: "memory_delete", Input: map[string]any{"path": existing.Path}})
				results <- result
				failures <- err
			}()
			approval := pending(t, e)
			if _, err = e.memories.Read(ctx, bot.ID, existing.Path); err != nil {
				t.Fatal("审批前删除了记忆", err)
			}
			choice, code := "approve", ""
			if decision == "deny" {
				choice = "deny"
				code = "TOOL_DENIED"
			}
			if decision == "changed" {
				code = "TOOL_TARGET_CHANGED"
				if _, err = e.memories.Write(ctx, bot.ID, existing.Path, domain.MemoryInput{Title: existing.Title, Content: "等待审批时的新偏好", Importance: 4}, existing.SHA256, "user"); err != nil {
					t.Fatal(err)
				}
			}
			if err = e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: choice}, "user"); err != nil {
				t.Fatal(err)
			}
			result := <-results
			if err = <-failures; err != nil {
				t.Fatal(err)
			}
			if code != "" {
				if !result.IsError || !strings.Contains(result.Content, code) {
					t.Fatal(result)
				}
				if _, err = e.memories.Read(ctx, bot.ID, existing.Path); err != nil {
					t.Fatal("拒绝或版本变化仍删除", err)
				}
			} else if result.IsError || !strings.Contains(result.Content, `"deleted":true`) {
				t.Fatal(result)
			}
		})
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
