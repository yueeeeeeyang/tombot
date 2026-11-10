package service

import (
	"context"
	"errors"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
	"sync"
	"testing"
)

func groupFixture(t *testing.T) (*Groups, *Bots, []domain.Bot) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	groups := NewGroups(db, events)
	bots := NewBots(db, events, t.TempDir())
	members := []domain.Bot{}
	for _, template := range []string{"researcher", "writer", "assistant"} {
		bot, err := bots.Create(context.Background(), BotInput{Template: template}, "user")
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, bot)
	}
	return groups, bots, members
}
func groupError(t *testing.T, err error, code string) {
	t.Helper()
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != code {
		t.Fatalf("预期 %s，得到 %v", code, err)
	}
}

func TestGroupCRUDRevisionMembershipAndConversation(t *testing.T) {
	ctx := context.Background()
	groups, bots, members := groupFixture(t)
	group, err := groups.Create(ctx, GroupInput{Name: " 方案讨论 ", Purpose: "共享研究与写作", CoordinatorBotID: &members[0].ID, Members: []string{members[0].ID, members[1].ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if group.Name != "方案讨论" || group.MaxSpeakers != 3 || group.MaxCostMicro != domain.DefaultGroupBudgetMicro || len(group.Members) != 2 || !group.AutoSummary {
		t.Fatalf("群默认值错误：%+v", group)
	}
	conversation, err := NewConversations(groups.db, groups.events).Get(ctx, group.ConversationID)
	if err != nil || conversation.Kind != "group" || len(conversation.Members) != 2 || conversation.Title != group.Name {
		t.Fatalf("群会话或色带元数据错误：%+v %v", conversation, err)
	}
	group, err = groups.ChangeMembers(ctx, group.ID, GroupMembersInput{Revision: group.Revision, BotIDs: []string{members[2].ID}, RoleNotes: map[string]string{members[2].ID: "跟进结论"}}, "", "user")
	if err != nil || len(group.Members) != 3 {
		t.Fatal(err)
	}
	_, err = groups.Update(ctx, group.ID, GroupPatch{Revision: 1, Name: Field[string]{Set: true, Value: "旧编辑"}}, "user")
	groupError(t, err, "GROUP_CHANGED")
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := groups.Update(ctx, group.ID, GroupPatch{Revision: group.Revision, Purpose: Field[string]{Set: true, Value: "确认用途"}}, "user")
			outcomes <- err
		}()
	}
	wg.Wait()
	close(outcomes)
	success := 0
	for err := range outcomes {
		if err == nil {
			success++
		} else {
			groupError(t, err, "GROUP_CHANGED")
		}
	}
	if success != 1 {
		t.Fatal("并发编辑未排他")
	}
	group, err = groups.Get(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = groups.ChangeMembers(ctx, group.ID, GroupMembersInput{Revision: group.Revision}, members[0].ID, "user")
	groupError(t, err, "GROUP_COORDINATOR_REQUIRED")
	if err = bots.Delete(ctx, members[0].ID, members[0].Name, "user", false); err != nil {
		t.Fatal(err)
	}
	latest, err := groups.Get(ctx, group.ID)
	if err != nil || latest.CoordinatorBotID != nil || len(latest.Members) != 2 || latest.Revision != group.Revision+1 {
		t.Fatalf("删除成员未同步群配置：%+v %v", latest, err)
	}
	if err = groups.Delete(ctx, latest.ID, latest.Revision, "user"); err != nil {
		t.Fatal(err)
	}
	_, err = NewConversations(groups.db, groups.events).Get(ctx, latest.ConversationID)
	groupError(t, err, "NOT_FOUND")
	if _, err = bots.Get(ctx, members[1].ID); err != nil {
		t.Fatal("删群误删成员", err)
	}
}

func TestGroupInvalidInputAndAuditFailureRollback(t *testing.T) {
	ctx := context.Background()
	groups, bots, members := groupFixture(t)
	for _, input := range []GroupInput{
		{Name: "无成员"}, {Name: "重复", Members: []string{members[0].ID, members[0].ID}},
		{Name: "缺协调者", Routing: "coordinator", Members: []string{members[0].ID}},
		{Name: "越权协调者", CoordinatorBotID: &members[1].ID, Members: []string{members[0].ID}},
	} {
		if _, err := groups.Create(ctx, input, "user"); err == nil {
			t.Fatal("无效群被创建", input.Name)
		}
	}
	policy := members[1].MessagingPolicy
	policy.AcceptGroupInvite = false
	if _, err := bots.Update(ctx, members[1].ID, BotPatch{MessagingPolicy: Field[domain.MessagingPolicy]{Set: true, Value: policy}}, "user"); err != nil {
		t.Fatal(err)
	}
	_, err := groups.Create(ctx, GroupInput{Name: "邀请拒绝", Members: []string{members[0].ID, members[1].ID}}, "user")
	groupError(t, err, "GROUP_INVITE_DENIED")
	if _, err = groups.Create(ctx, GroupInput{Name: "直接创建", Members: []string{members[0].ID}}, "user"); err != nil {
		t.Fatal(err)
	}
	var count int
	for _, query := range []string{"SELECT COUNT(*) FROM groups", "SELECT COUNT(*) FROM group_members", "SELECT COUNT(*) FROM conversations WHERE kind='group'"} {
		if err = groups.db.QueryRow(query).Scan(&count); err != nil || count != 1 {
			t.Fatal("群创建未提交", query, err, count)
		}
	}
}

func TestGroupActiveTurnBlocksMutationBeforeAnyRun(t *testing.T) {
	ctx := context.Background()
	groups, bots, members := groupFixture(t)
	group, err := groups.Create(ctx, GroupInput{Name: "活动群", Members: []string{members[0].ID, members[1].ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = groups.db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('group-turn',?,1,'user','queued',0)`, group.ConversationID); err != nil {
		t.Fatal(err)
	}
	_, err = groups.Update(ctx, group.ID, GroupPatch{Revision: 1}, "user")
	groupError(t, err, "TURN_IN_PROGRESS")
	_, err = groups.ChangeMembers(ctx, group.ID, GroupMembersInput{Revision: 1, BotIDs: []string{members[2].ID}}, "", "user")
	groupError(t, err, "TURN_IN_PROGRESS")
	groupError(t, groups.Delete(ctx, group.ID, 1, "user"), "TURN_IN_PROGRESS")
	_, err = bots.Update(ctx, members[0].ID, BotPatch{Name: Field[string]{Set: true, Value: "不能改"}}, "user")
	groupError(t, err, "BOT_BUSY")
	groupError(t, bots.Delete(ctx, members[1].ID, members[1].Name, "user", false), "BOT_BUSY")
	conversation, err := NewConversations(groups.db, groups.events).Get(ctx, group.ConversationID)
	if err != nil || conversation.BotStatus != "running" {
		t.Fatal("没有 Run 的群回合被误判为空闲", err, conversation.BotStatus)
	}
}
