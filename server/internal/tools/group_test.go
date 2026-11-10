package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
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
	finished := make(chan domain.ToolResultBlock, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := e.Execute(context.Background(), domain.ToolInvocation{Run: run, Bot: bot, Step: 0}, domain.ToolUseBlock{ID: ids.New(), Name: "post_to_group", Input: map[string]any{"group_id": group.ID, "message": "需要许可的发言"}})
		finished <- result
		failed <- err
	}()
	approval := pending(t, e)
	if err := e.approvals.Decide(context.Background(), approval.ID, service.ApprovalDecision{Decision: "deny"}, "user"); err != nil {
		t.Fatal(err)
	}
	if result := <-finished; !result.IsError || !strings.Contains(result.Content, "TOOL_DENIED") {
		t.Fatal(result)
	}
	if err := <-failed; err != nil {
		t.Fatal(err)
	}
	bot.ToolPolicy.Approval = "standard"
	if _, err := e.db.Exec(`CREATE TRIGGER fail_group_post_audit BEFORE INSERT ON audit_log WHEN NEW.action='group.post' BEGIN SELECT RAISE(ABORT,'test_group_post_audit'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := e.Execute(context.Background(), domain.ToolInvocation{Run: run, Bot: bot, Step: 1}, domain.ToolUseBlock{ID: ids.New(), Name: "post_to_group", Input: map[string]any{"group_id": group.ID, "message": "审计失败必须回滚"}})
	if err == nil {
		t.Fatal("审计失败被吞掉")
	}
	var count int
	if err = e.db.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=?", group.ConversationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("拒绝或审计失败后仍然发布消息", count, err)
	}
}
