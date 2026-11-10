package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func checkDefaultInitializationCounts(t *testing.T, db *sql.DB, created, pending int) {
	t.Helper()
	var bots, conversations, audits, events, seeds int
	if err := db.QueryRow(`SELECT
(SELECT COUNT(*) FROM bots WHERE id='tom'),
(SELECT COUNT(*) FROM conversations WHERE id='tom-direct' AND kind='direct' AND bot_id='tom'),
(SELECT COUNT(*) FROM audit_log WHERE action='bot.create' AND target='tom'),
(SELECT COUNT(*) FROM events WHERE kind='conversation.updated' AND conversation_id='tom-direct'),
(SELECT COUNT(*) FROM system_state WHERE key='default_bot.pending')`).Scan(&bots, &conversations, &audits, &events, &seeds); err != nil {
		t.Fatal(err)
	}
	if bots != created || conversations != created || audits != created || events != created || seeds != pending {
		t.Fatal("默认助手创建、审计、事件和种子消费不同步", bots, conversations, audits, events, seeds)
	}
}

func TestDefaultBotInitializationPreservesExistingNamesAndUsesCurrentModel(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	bots := NewBots(db, bus.New(db), workspace)
	var existing []domain.Bot
	for _, name := range []string{"Tom", "Tom 1"} {
		created, err := bots.Create(ctx, BotInput{Template: "writer", Name: name, Soul: "Preserve the user's custom instructions."}, "user")
		if err != nil {
			t.Fatal(err)
		}
		before, err := bots.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		existing = append(existing, before)
		if err = os.WriteFile(filepath.Join(workspace, before.WorkspaceDir, "notes", "kept.txt"), []byte("用户资料"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := NewCatalog(db)
	provider, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "测试供应商", BaseURL: "http://127.0.0.1:1234/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	model, err := catalog.CreateModel(ctx, ModelInput{ProviderID: provider.ID, ModelKey: "configured", DisplayName: "已有默认模型", ContextWindow: 8192, MaxOutput: 1024, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	tom, err := bots.Get(ctx, DefaultBotID)
	if err != nil || tom.Name != "Tom 2" || tom.ModelID == nil || *tom.ModelID != model.ID {
		t.Fatal("未避开已有名称或未绑定消费前配置的默认模型", tom, err)
	}
	for _, before := range existing {
		after, err := bots.Get(ctx, before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("初始化修改了用户已有 Bot", before.ID, after, err)
		}
		content, err := os.ReadFile(filepath.Join(workspace, before.WorkspaceDir, "notes", "kept.txt"))
		if err != nil || string(content) != "用户资料" {
			t.Fatal("初始化破坏了用户资料", before.ID, err)
		}
	}
	checkDefaultInitializationCounts(t, db, 1, 0)
}

func TestDefaultBotInitializationRollsBackAndRetriesConversationFailure(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	bots := NewBots(db, bus.New(db), workspace)
	var seed string
	if err := db.QueryRow(`SELECT value_json FROM system_state WHERE key='default_bot.pending'`).Scan(&seed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_default_conversation BEFORE INSERT ON conversations WHEN NEW.id='tom-direct' BEGIN SELECT RAISE(ABORT,'fixture rejection'); END`); err != nil {
		t.Fatal(err)
	}
	if err := bots.EnsureDefault(ctx); err == nil || !strings.Contains(err.Error(), DefaultBotID) {
		t.Fatal("主会话失败未明确报告默认助手初始化错误", err)
	}
	checkDefaultInitializationCounts(t, db, 0, 1)
	var remaining string
	if err := db.QueryRow(`SELECT value_json FROM system_state WHERE key='default_bot.pending'`).Scan(&remaining); err != nil || remaining != seed {
		t.Fatal("失败事务消费或修改了种子", err)
	}
	for _, part := range []string{"", "inbox", "out", "notes", ".tom/memory"} {
		entry, err := os.Stat(filepath.Join(workspace, "bots", "tom", part))
		if err != nil || !entry.IsDir() || entry.Mode().Perm() != 0700 {
			t.Fatal("失败后预备目录不可安全复用", part, err)
		}
	}
	kept := filepath.Join(workspace, "bots", "tom", "notes", "kept.txt")
	if err := os.WriteFile(kept, []byte("重试前资料"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_default_conversation`); err != nil {
		t.Fatal(err)
	}
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal("解除事务失败后无法重试", err)
	}
	checkDefaultInitializationCounts(t, db, 1, 0)
	if content, err := os.ReadFile(kept); err != nil || string(content) != "重试前资料" {
		t.Fatal("重试清除了已有目录资料", err)
	}
}

func TestDefaultBotInitializationConcurrentCallsConsumeOnce(t *testing.T) {
	db := testkit.Database(t)
	bots := NewBots(db, bus.New(db), t.TempDir())
	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			results <- bots.EnsureDefault(context.Background())
		}()
	}
	close(start)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal("并发初始化失败", err)
		}
	}
	for range 3 {
		if err := bots.EnsureDefault(context.Background()); err != nil {
			t.Fatal("重复初始化失败", err)
		}
	}
	checkDefaultInitializationCounts(t, db, 1, 0)
}

func TestDefaultBotInitializationRejectsCorruptPendingWithoutChanges(t *testing.T) {
	for _, encoded := range []string{"{invalid", `{"id":"tom","name":"Tom"}`} {
		t.Run(encoded, func(t *testing.T) {
			db := testkit.Database(t)
			workspace := t.TempDir()
			bots := NewBots(db, bus.New(db), workspace)
			if _, err := db.Exec(`UPDATE system_state SET value_json=? WHERE key='default_bot.pending'`, encoded); err != nil {
				t.Fatal(err)
			}
			if err := bots.EnsureDefault(context.Background()); err == nil || !strings.Contains(err.Error(), "种子无效") {
				t.Fatal("损坏种子没有明确失败", err)
			}
			checkDefaultInitializationCounts(t, db, 0, 1)
			var unchanged string
			if err := db.QueryRow(`SELECT value_json FROM system_state WHERE key='default_bot.pending'`).Scan(&unchanged); err != nil || unchanged != encoded {
				t.Fatal("失败覆盖了原始种子", err)
			}
			if _, err := os.Stat(filepath.Join(workspace, "bots", "tom")); !os.IsNotExist(err) {
				t.Fatal("损坏种子仍创建了工作区", err)
			}
		})
	}
}

func TestDefaultBotInitializationSteadyStateDoesNotTouchWorkspace(t *testing.T) {
	db := testkit.Database(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	bots := NewBots(db, bus.New(db), workspace)
	if err := bots.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, filepath.Join(root, "kept-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := bots.EnsureDefault(context.Background()); err != nil {
		t.Fatal("已初始化助手仍访问工作区", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("已消费种子仍重新创建工作区", err)
	}
	checkDefaultInitializationCounts(t, db, 1, 0)
}
