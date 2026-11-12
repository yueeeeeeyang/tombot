package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

func groupToolFixture(t *testing.T, approval string) (*Executor, domain.Bot, domain.Run, domain.Group) {
	t.Helper()
	executor, bot, run := fixture(t, approval)
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "group_members", "post_to_group")
	group, err := service.NewGroups(executor.db, executor.events).Create(context.Background(), service.GroupInput{Name: "工具协作群", Members: []string{bot.ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executor.db.Exec("UPDATE turns SET conversation_id=? WHERE id=?", group.ConversationID, run.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err = executor.db.Exec("UPDATE runs SET conversation_id=?,source='group',plan_role='member' WHERE id=?", group.ConversationID, run.ID); err != nil {
		t.Fatal(err)
	}
	run.ConversationID = group.ConversationID
	run.Source = "group"
	run.PlanRole = "member"
	return executor, bot, run, group
}

func TestGroupToolsMembershipPostAndLoopRejection(t *testing.T) {
	e, bot, run, group := groupToolFixture(t, "standard")
	result := execute(t, e, bot, run, "group_members", map[string]any{"group_id": group.ID})
	var members []domain.GroupMember
	if err := json.Unmarshal([]byte(result.Content), &members); err != nil || result.IsError || len(members) != 1 || members[0].BotID != bot.ID || strings.Contains(result.Content, bot.Soul) {
		t.Fatal("成员资料错误或泄露私有提示", result, err)
	}
	result = execute(t, e, bot, run, "post_to_group", map[string]any{"group_id": group.ID, "message": "产物位于 shared/report.md"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	messages, err := service.NewConversations(e.db, e.events).Messages(context.Background(), group.ConversationID, 0, 50)
	if err != nil || len(messages) != 1 || messages[0].TurnID == nil || *messages[0].TurnID != run.TurnID || *messages[0].SenderBotID != bot.ID {
		t.Fatal("消息归属错误", messages, err)
	}
	var turns int
	if err = e.db.QueryRow("SELECT COUNT(*) FROM turns").Scan(&turns); err != nil || turns != 1 {
		t.Fatal("追加消息创建了新回合", turns, err)
	}
	for _, input := range []map[string]any{{"group_id": "outsider", "message": "越权"}, {"group_id": group.ID, "message": " "}, {"group_id": group.ID, "message": strings.Repeat("x", 16385)}} {
		if result = execute(t, e, bot, run, "post_to_group", input); !result.IsError {
			t.Fatal("无效群消息被发布", input)
		}
	}
	if _, err = e.db.Exec("UPDATE runs SET source='bot_message' WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	result = execute(t, e, bot, run, "post_to_group", map[string]any{"group_id": group.ID, "message": "私信回到群"})
	if !result.IsError || !strings.Contains(result.Content, "GROUP_POST_CONTEXT") {
		t.Fatal("私信回群环路未被阻止", result)
	}
	var count int
	if err = e.db.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", group.ConversationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("失败调用留下公开发言", count, err)
	}
}

func TestGroupPostApprovalAndAuditRollback(t *testing.T) {
	e, bot, run, group := groupToolFixture(t, "strict")
	result := execute(t, e, bot, run, "post_to_group", map[string]any{"group_id": group.ID, "message": "直接发布的发言"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	var count int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", group.ConversationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("严格策略没有立即发布消息", count, err)
	}
}
