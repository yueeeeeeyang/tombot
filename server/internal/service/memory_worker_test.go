package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryMaintenanceScansStartupAndPeriodicManualEdits(t *testing.T) {
	s, bot, _, root := memoryFixture(t)
	file := filepath.Join(root, bot.WorkspaceDir, ".tom/memory/manual.md")
	if err := os.WriteFile(file, []byte("启动时的记忆正文"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.maintain(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), ticks) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("记忆校验未停止")
		}
	})
	wait := func(expected string) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			wake := s.events.Wake()
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM memory_fts_cjk WHERE bot_id=? AND content=?", bot.ID, expected).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == 1 {
				return
			}
			select {
			case <-wake:
			case <-deadline.C:
				t.Fatal("后台校验没有同步文件", expected)
			}
		}
	}
	wait("启动时的记忆正文")
	if err := os.WriteFile(file, []byte("定期检查同步的新内容"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("维护循环未等待定时信号")
	}
	wait("定期检查同步的新内容")
}

func TestShortMemoryQueryReturnsDeepMatchSnippetAndFullLengthFlag(t *testing.T) {
	s, bot, _, _ := memoryFixture(t)
	entry := putMemory(t, s, bot.ID, "", "长记录", strings.Repeat("普通记录内容。\n", 195)+"特定偏好在末尾出现。", 3)
	if !entry.TooLong {
		t.Fatal("未计入 front matter 行数")
	}
	found, err := s.Search(context.Background(), bot.ID, "偏好", 5)
	if err != nil || len(found) != 1 || !found[0].TooLong || !strings.Contains(found[0].Snippet, "特定偏好") {
		t.Fatal("短词片段或长度标记错误", found, err)
	}
}
