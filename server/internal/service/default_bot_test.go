package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/testkit"
)

func TestDefaultBotWorkspacePreservesCustomizationAndDeletion(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	bots := NewBots(db, bus.New(db), workspace)
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	bot, err := bots.Get(ctx, DefaultBotID)
	if err != nil || bot.Name != "Tom" || bot.ConversationID != "tom-direct" || bot.AvatarPath != nil || bot.ModelID != nil {
		t.Fatal("缺少默认助手身份", bot, err)
	}
	if err = validateBot(&bot); err != nil {
		t.Fatal("默认助手策略不符合普通 Bot 规则", err)
	}
	if err = bots.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"inbox", "out", "notes", ".tom/memory"} {
		entry, err := os.Stat(filepath.Join(workspace, bot.WorkspaceDir, part))
		if err != nil || !entry.IsDir() || entry.Mode().Perm() != 0700 {
			t.Fatal("默认助手工作区未准备完整", part, err)
		}
	}
	kept := filepath.Join(workspace, bot.WorkspaceDir, "notes", "user.txt")
	if err = os.WriteFile(kept, []byte("保留用户资料"), 0600); err != nil {
		t.Fatal(err)
	}
	bot, err = bots.Update(ctx, bot.ID, BotPatch{Name: Field[string]{Set: true, Value: "我的 Tom"}, Soul: Field[string]{Set: true, Value: "Customized instructions."}, IsArchived: Field[bool]{Set: true, Value: true}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	restarted := NewBots(db, bus.New(db), workspace)
	if err = restarted.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := restarted.Get(ctx, bot.ID)
	if err != nil || after.Name != bot.Name || after.Soul != bot.Soul || !after.IsArchived {
		t.Fatal("重启覆盖了用户修改或归档", after, err)
	}
	content, err := os.ReadFile(kept)
	if err != nil || string(content) != "保留用户资料" {
		t.Fatal("重启覆盖了工作区资料", err)
	}
	if err = restarted.Delete(ctx, bot.ID, bot.Name, "user", true); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = restarted.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Get(ctx, bot.ID); err == nil {
		t.Fatal("删除后默认助手重新出现")
	}
	if _, err = os.Stat(filepath.Join(workspace, bot.WorkspaceDir)); !os.IsNotExist(err) {
		t.Fatal("删除后工作区重新出现", err)
	}
}

func TestDefaultBotWorkspaceFailureIsVisibleAndRetryPreservesFiles(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	bots := NewBots(db, bus.New(db), workspace)
	directory := filepath.Join(workspace, "bots", "tom")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(directory, "out")
	if err := os.WriteFile(conflict, []byte("用户文件"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := bots.EnsureDefault(ctx); err == nil {
		t.Fatal("工作区目录被文件占用仍报告成功")
	}
	if content, err := os.ReadFile(conflict); err != nil || string(content) != "用户文件" {
		t.Fatal("准备失败破坏了用户文件", err)
	}
	if err := os.Rename(conflict, filepath.Join(directory, "kept.txt")); err != nil {
		t.Fatal(err)
	}
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal("恢复目录后无法重试", err)
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "bots"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(other, "bots", "tom")); err != nil {
		t.Fatal(err)
	}
	otherDB := testkit.Database(t)
	if err := NewBots(otherDB, bus.New(otherDB), other).EnsureDefault(ctx); err == nil {
		t.Fatal("工作区别名或逃逸未被拒绝")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("修改了工作区外目录", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := bots.EnsureDefault(canceled); err == nil {
		t.Fatal("取消后仍报告准备成功")
	}
}

func TestDefaultBotModelBindingIsTransactionalAndPreservesUserChoice(t *testing.T) {
	for _, usePatch := range []bool{false, true} {
		db := testkit.Database(t)
		ctx := context.Background()
		bots := NewBots(db, bus.New(db), t.TempDir())
		if err := bots.EnsureDefault(ctx); err != nil {
			t.Fatal(err)
		}
		ordinary, err := bots.Create(ctx, BotInput{Template: "writer"}, "user")
		if err != nil {
			t.Fatal(err)
		}
		catalog := NewCatalog(db)
		p, err := catalog.CreateProvider(ctx, ProviderInput{Kind: "openai_compat", DisplayName: "测试供应商", BaseURL: "http://127.0.0.1:1234/v1"}, "user")
		if err != nil {
			t.Fatal(err)
		}
		input := ModelInput{ProviderID: p.ID, ModelKey: "first", DisplayName: "首个模型", ContextWindow: 8192, MaxOutput: 1024, IsDefault: !usePatch}
		first, err := catalog.CreateModel(ctx, input, "user")
		if err != nil {
			t.Fatal(err)
		}
		if usePatch {
			if _, err = catalog.UpdateModel(ctx, first.ID, ModelPatch{IsDefault: Field[bool]{Set: true, Value: true}}, "user"); err != nil {
				t.Fatal(err)
			}
		}
		tom, err := bots.Get(ctx, DefaultBotID)
		if err != nil || tom.ModelID == nil || *tom.ModelID != first.ID {
			t.Fatal("默认助手未在设置默认模型时完成绑定", err)
		}
		ordinary, err = bots.Get(ctx, ordinary.ID)
		if err != nil || ordinary.ModelID != nil {
			t.Fatal("改变了其他 Bot 的未配置语义", err)
		}
		input.ModelKey, input.IsDefault = "second", true
		second, err := catalog.CreateModel(ctx, input, "user")
		if err != nil {
			t.Fatal(err)
		}
		tom, err = bots.Get(ctx, DefaultBotID)
		if err != nil || tom.ModelID == nil || *tom.ModelID != first.ID {
			t.Fatal("更换全局默认覆盖了已选模型", err)
		}
		if _, err = bots.Update(ctx, DefaultBotID, BotPatch{ModelID: Field[*string]{Set: true, Value: &second.ID}}, "user"); err != nil {
			t.Fatal(err)
		}
		if _, err = catalog.UpdateModel(ctx, first.ID, ModelPatch{IsDefault: Field[bool]{Set: true, Value: true}}, "user"); err != nil {
			t.Fatal(err)
		}
		tom, err = bots.Get(ctx, DefaultBotID)
		if err != nil || tom.ModelID == nil || *tom.ModelID != second.ID {
			t.Fatal("用户自选模型被覆盖", err)
		}
		if _, err = bots.Update(ctx, DefaultBotID, BotPatch{ModelID: Field[*string]{Set: true}}, "user"); err != nil {
			t.Fatal(err)
		}
		if _, err = catalog.UpdateModel(ctx, first.ID, ModelPatch{DisplayName: Field[string]{Set: true, Value: "修改显示名称"}}, "user"); err != nil {
			t.Fatal(err)
		}
		tom, err = bots.Get(ctx, DefaultBotID)
		if err != nil || tom.ModelID != nil {
			t.Fatal("普通模型元数据修改覆盖了用户清空的模型", err)
		}
		if _, err = db.Exec(`CREATE TRIGGER reject_default_model BEFORE UPDATE OF model_id ON bots WHEN NEW.id='tom' AND NEW.model_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'fixture rejection'); END`); err != nil {
			t.Fatal(err)
		}
		input.ModelKey = "rejected"
		if _, err = catalog.CreateModel(ctx, input, "user"); err == nil {
			t.Fatal("助手模型绑定失败未回滚模型创建")
		}
		var count int
		if err = db.QueryRow(`SELECT COUNT(*) FROM models WHERE model_key='rejected'`).Scan(&count); err != nil || count != 0 {
			t.Fatal("失败事务残留模型", count, err)
		}
		if _, err = catalog.UpdateModel(ctx, second.ID, ModelPatch{IsDefault: Field[bool]{Set: true, Value: true}}, "user"); err == nil {
			t.Fatal("助手模型绑定失败未回滚默认模型切换")
		}
		current, err := catalog.Model(ctx, first.ID)
		if err != nil || !current.IsDefault {
			t.Fatal("失败事务改变了原默认模型", err)
		}
	}
}
