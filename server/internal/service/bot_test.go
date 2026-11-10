package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestBotWorkspaceConversationRollbackAndDeletion(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	workspace := t.TempDir()
	bots := NewBots(db, events, workspace)
	bot, err := bots.Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if bot.ModelID != nil || bot.ConversationID == "" || bot.AccentHue < 0 || bot.AccentHue > 359 {
		t.Fatal("无模型 Bot 初始化错误")
	}
	for _, part := range []string{"inbox", "out", "notes", ".tom/memory"} {
		if _, err = os.Stat(filepath.Join(workspace, bot.WorkspaceDir, part)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = bots.Create(ctx, BotInput{Template: "researcher"}, "user"); err == nil {
		t.Fatal("重复岗位名被接受")
	}
	entries, err := os.ReadDir(filepath.Join(workspace, "bots"))
	if err != nil || len(entries) != 1 {
		t.Fatal("失败事务遗留工作区")
	}
	conversations := NewConversations(db, events)
	direct, err := conversations.Direct(ctx, bot.ID)
	if err != nil || direct.ID != bot.ConversationID {
		t.Fatal("Bot 没有唯一单聊")
	}
	if _, err = bots.Update(ctx, bot.ID, BotPatch{Name: Field[string]{Set: true, Value: "研究伙伴"}}, "user"); err != nil {
		t.Fatal(err)
	}
	direct, err = conversations.Get(ctx, direct.ID)
	if err != nil || direct.Title != "研究伙伴" {
		t.Fatal("会话标题未同步")
	}
	if err = bots.Delete(ctx, bot.ID, "错误名称", "user", false); err == nil {
		t.Fatal("没有确认就删除")
	}
	if err = bots.Delete(ctx, bot.ID, "研究伙伴", "user", false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(workspace, bot.WorkspaceDir)); err != nil {
		t.Fatal("默认删除不应清理文件")
	}
	if _, err = conversations.Get(ctx, direct.ID); err == nil {
		t.Fatal("删除 Bot 未删除单聊")
	}
}
func TestBotDirectoryCannotEscapeWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "bots")); err != nil {
		t.Fatal(err)
	}
	db := testkit.Database(t)
	bots := NewBots(db, bus.New(db), workspace)
	if _, err := bots.Create(context.Background(), BotInput{Template: "writer"}, "user"); err == nil {
		t.Fatal("软链逃逸未拒绝")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("修改了工作区外目录")
	}
}
func TestAvatarValidationAndPurge(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	workspace := t.TempDir()
	bots := NewBots(db, bus.New(db), workspace)
	bot, err := bots.Create(ctx, BotInput{Template: "assistant"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bots.SetAvatar(ctx, bot.ID, "user", bytes.NewBufferString("<svg/>")); err == nil {
		t.Fatal("可执行头像格式被接受")
	}
	picture := image.NewRGBA(image.Rect(0, 0, 8, 8))
	picture.Set(0, 0, color.White)
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	if _, err = bots.SetAvatar(ctx, bot.ID, "user", &encoded); err != nil {
		t.Fatal(err)
	}
	file, err := bots.Avatar(ctx, bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = png.Decode(file); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = bots.Delete(ctx, bot.ID, bot.Name, "user", true); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(workspace, bot.WorkspaceDir)); !os.IsNotExist(err) {
		t.Fatal("确认清理后工作区仍存在")
	}
}
func TestDeletingCoordinatorPreservesGroup(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	bots := NewBots(db, bus.New(db), t.TempDir())
	bot, err := bots.Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO groups(id,name,coordinator_bot_id,created_at,updated_at) VALUES('group','测试群',?,0,0); INSERT INTO group_members(group_id,bot_id,joined_at) VALUES('group',?,0)`, bot.ID, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err = bots.Delete(ctx, bot.ID, bot.Name, "user", false); err != nil {
		t.Fatal(err)
	}
	var coordinator *string
	if err = db.QueryRow("SELECT coordinator_bot_id FROM groups WHERE id='group'").Scan(&coordinator); err != nil || coordinator != nil {
		t.Fatal("群组协调者未清空")
	}
}

func TestAutomaticBotHueUsesUnoccupiedArcAndRespectsExplicitChoice(t *testing.T) {
	db := testkit.Database(t)
	bots := NewBots(db, bus.New(db), t.TempDir())
	ctx := context.Background()
	fixed := 350
	first, err := bots.Create(ctx, BotInput{Template: "researcher", AccentHue: &fixed}, "user")
	if err != nil || first.AccentHue != 350 {
		t.Fatal("手选色相未保留", err)
	}
	second, err := bots.Create(ctx, BotInput{Template: "writer"}, "user")
	if err != nil || second.AccentHue != 170 {
		t.Fatal("未选择跨零度空隙中点", err)
	}
	third, err := bots.Create(ctx, BotInput{Template: "assistant"}, "user")
	if err != nil || third.AccentHue != 260 {
		t.Fatal("三个岗位未自动分色", err)
	}
	_, err = bots.Update(ctx, second.ID, BotPatch{IsArchived: Field[bool]{Set: true, Value: true}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := bots.Create(ctx, BotInput{Template: "writer", Name: "校对员"}, "user")
	if err != nil || fourth.AccentHue != 125 {
		t.Fatal("归档岗位仍占用活动色环", err)
	}
}
