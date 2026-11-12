package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/workspace"
)

func memoryFixture(t *testing.T) (*Memories, domain.Bot, domain.Bot, string) {
	t.Helper()
	root := t.TempDir()
	db := testkit.Database(t)
	events := bus.New(db)
	bots := NewBots(db, events, root)
	first, err := bots.Create(context.Background(), BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	second, err := bots.Create(context.Background(), BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	files, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewMemories(db, bots, files, events), first, second, root
}

func putMemory(t *testing.T, s *Memories, botID, path, title, body string, importance int) domain.Memory {
	t.Helper()
	entry, err := s.Write(context.Background(), botID, path, domain.MemoryInput{Title: title, Content: body, Importance: importance}, "", "user")
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestMemorySearchEnglishChineseShortQueryAndPrivacy(t *testing.T) {
	s, first, second, _ := memoryFixture(t)
	ctx := context.Background()
	english := putMemory(t, s, first.ID, "", "Research practices", "Running source checks before publishing reports.", 3)
	chinese := putMemory(t, s, first.ID, "", "信源偏好", "优先官方文档，并核验可信来源。", 4)
	putMemory(t, s, second.ID, "", "同事私有资料", "Running private projects; 优先官方文档。", 5)
	for _, test := range []struct{ query, path string }{{"runs", english.Path}, {"官方文档", chinese.Path}, {"偏好", chinese.Path}, {"官方", chinese.Path}} {
		found, err := s.Search(ctx, first.ID, test.query, 5)
		if err != nil || len(found) != 1 || found[0].Path != test.path {
			t.Fatal(test, found, err)
		}
	}
	found, err := s.Search(ctx, first.ID, "private", 5)
	if err != nil || len(found) != 0 {
		t.Fatal("跨 Bot 记忆泄漏", found, err)
	}
	if _, err = s.Search(ctx, first.ID, "\" OR *", 5); err != nil {
		t.Fatal("文字被当作 FTS 表达式", err)
	}
	prompt, err := s.PromptIndex(ctx, first.ID)
	if err != nil || len(prompt) > domain.MaxMemoryIndexBytes || strings.Contains(prompt, "同事私有资料") || !strings.Contains(prompt, chinese.Path) {
		t.Fatal(prompt, err)
	}
}

func TestMemoryImportanceRecencyAndRebuildAfterManualChanges(t *testing.T) {
	s, bot, _, root := memoryFixture(t)
	ctx := context.Background()
	paths := []string{".tom/memory/important.md", ".tom/memory/ordinary.md", ".tom/memory/old.md"}
	for index, path := range paths {
		importance := 5
		if index == 1 {
			importance = 1
		}
		putMemory(t, s, bot.ID, path, "Reference", "ranking marker for reference", importance)
	}
	old := filepath.Join(root, bot.WorkspaceDir, paths[2])
	past := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	found, err := s.Search(ctx, bot.ID, "ranking", 5)
	if err != nil || len(found) != 3 {
		t.Fatal(found, err)
	}
	for index, path := range paths {
		if found[index].Path != path {
			t.Fatal("重要性或半衰排序不正确", found)
		}
	}
	changed := filepath.Join(root, bot.WorkspaceDir, paths[0])
	if err = os.WriteFile(changed, []byte("手改文件采用首行摘要。\n只保留新的检索词。"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = s.Sync(ctx, bot.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	found, err = s.Search(ctx, bot.ID, "检索词", 5)
	if err != nil || len(found) != 1 || found[0].Title != "important" {
		t.Fatal("手改没有校正", found, err)
	}
	for _, table := range []string{"memory_index", "memory_fts_ascii", "memory_fts_cjk"} {
		var count int
		if err = s.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE bot_id=?", bot.ID).Scan(&count); err != nil || count != 3 {
			t.Fatal("重建不是幂等", table, count, err)
		}
	}
	if err = os.Remove(changed); err != nil {
		t.Fatal(err)
	}
	found, err = s.Search(ctx, bot.ID, "检索词", 5)
	if err != nil || len(found) != 0 {
		t.Fatal("已删文件仍可检索", found, err)
	}
}

func TestMemoryConcurrentEditAndAuditFailure(t *testing.T) {
	s, bot, _, _ := memoryFixture(t)
	ctx := context.Background()
	entry := putMemory(t, s, bot.ID, "", "一致性", "原文", 3)
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for _, body := range []string{"修改甲", "修改乙"} {
		workers.Go(func() {
			_, err := s.Write(ctx, bot.ID, entry.Path, domain.MemoryInput{Title: entry.Title, Content: body, Importance: 3}, entry.SHA256, "user")
			errorsFound <- err
		})
	}
	workers.Wait()
	close(errorsFound)
	successes := 0
	for err := range errorsFound {
		if err == nil {
			successes++
		} else {
			var public *domain.Error
			if !errors.As(err, &public) || public.Code != "MEMORY_CHANGED" {
				t.Fatal(err)
			}
		}
	}
	if successes != 1 {
		t.Fatal("并发覆盖", successes)
	}
	if err := s.Delete(ctx, bot.ID, entry.Path, entry.SHA256, "user"); err == nil {
		t.Fatal("旧摘要允许删除")
	}
	current, err := s.Read(ctx, bot.ID, entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, bot.ID, current.Path, current.SHA256, "user"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"memory_index", "memory_fts_ascii", "memory_fts_cjk"} {
		var count int
		if err = s.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE bot_id=?", bot.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}

func TestMemoryIndexFailurePreservesAuthoritativeFileAndRecovers(t *testing.T) {
	s, bot, _, _ := memoryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_memory_index BEFORE INSERT ON memory_index BEGIN SELECT RAISE(ABORT,'test_memory_index'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := s.Write(ctx, bot.ID, ".tom/memory/recovery.md", domain.MemoryInput{Title: "恢复", Content: "已经落盘的恢复线索", Importance: 3}, "", "user")
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != "MEMORY_SAVED_INDEX_FAILED" {
		t.Fatal("索引失败被掩盖", err)
	}
	if _, err = s.files.ReadMemory(ctx, bot.ID, ".tom/memory/recovery.md"); err != nil {
		t.Fatal("权威文件被丢弃", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM memory_fts_cjk").Scan(&count); err != nil || count != 0 {
		t.Fatal("派生事务部分提交", count, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_memory_index"); err != nil {
		t.Fatal(err)
	}
	found, err := s.Search(ctx, bot.ID, "恢复线索", 5)
	if err != nil || len(found) != 1 {
		t.Fatal("未能自动校正", found, err)
	}
	var used sql.NullInt64
	if err = s.db.QueryRow("SELECT last_used_at FROM memory_index WHERE bot_id=?", bot.ID).Scan(&used); err != nil || !used.Valid {
		t.Fatal("未记录实际使用", used, err)
	}
}
