package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func TestGroupMentionsRoutePeersWithoutLeakingPrivateHistory(t *testing.T) {
	var firstName, secondName, thirdName string
	var secondCalls, thirdCalls atomic.Int32
	const private = "本条只给第一只猫咪的独有内容"
	r, first := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		encoded, err := json.Marshal(req.Messages)
		if err != nil {
			return provider.Usage{}, err
		}
		all := req.System + string(encoded)
		response := ""
		if strings.Contains(req.System, "Name: "+firstName+"\n") {
			response = "@" + secondName + " 请检查明确交给你的这一句话。"
		} else if strings.Contains(req.System, "Name: "+secondName+"\n") {
			secondCalls.Add(1)
			if strings.Contains(all, private) || !strings.Contains(all, "请检查明确交给你的这一句话") {
				t.Error("被提及成员收到了原始私有消息或未收到明确转交内容")
			}
			response = "@用户 已检查完成。"
		} else if strings.Contains(req.System, "Name: "+thirdName+"\n") {
			thirdCalls.Add(1)
			if strings.Contains(all, private) || strings.Contains(all, "请检查明确交给你的这一句话") || strings.Contains(all, "已检查完成") {
				t.Error("后续成员上下文泄露了私有提及")
			}
			response = "收到新的请求。"
		} else {
			t.Error("未知成员身份")
		}
		return provider.Usage{InputTokens: 1}, emit(provider.Event{Kind: "text_delta", Text: response})
	}), DefaultLimits())
	group := createTestGroup(t, r, first, domain.DefaultGroupBudgetMicro)
	firstName = first.Name
	others := []domain.GroupMember{}
	for _, member := range group.Members {
		if member.BotID != first.ID {
			others = append(others, member)
		}
	}
	secondName, thirdName = others[0].Name, others[1].Name
	ctx := context.Background()
	sent, err := r.Submit(ctx, group.ConversationID, SendInput{Content: "@" + firstName + " " + private}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if status, code, _ := waitGroupTurn(t, r, sent.TurnID); status != "succeeded" {
		t.Fatal(status, code)
	}
	if secondCalls.Load() != 1 || thirdCalls.Load() != 0 {
		t.Fatal("提及路由不正确", secondCalls.Load(), thirdCalls.Load())
	}
	next, err := r.Submit(ctx, group.ConversationID, SendInput{Content: "下一件事情", Mentions: []string{others[1].BotID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if status, code, _ := waitGroupTurn(t, r, next.TurnID); status != "succeeded" {
		t.Fatal(status, code)
	}
	conversations := service.NewConversations(r.deps.DB, r.deps.Events)
	transcript, err := conversations.ReadTranscript(ctx, others[1].BotID, domain.ConversationRead{ConversationID: group.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range transcript.Messages {
		if strings.Contains(message.Text, private) || strings.Contains(message.Text, "已检查完成") {
			t.Fatal("读取工具绕过了提及可见范围")
		}
	}
	hits, err := conversations.SearchTranscript(ctx, others[1].BotID, domain.ConversationSearch{Query: "独有内容", Kind: "group"})
	if err != nil || len(hits) != 0 {
		t.Fatal("搜索工具泄露私有消息", hits, err)
	}
	_, err = conversations.ReadTranscript(ctx, others[1].BotID, domain.ConversationRead{ConversationID: group.ConversationID, AroundMessageID: sent.MessageID})
	requireCode(t, err, "CONVERSATION_MESSAGE_NOT_FOUND")
	visible, err := conversations.ReadTranscript(ctx, first.ID, domain.ConversationRead{ConversationID: group.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range visible.Messages {
		found = found || strings.Contains(message.Text, private)
	}
	if !found {
		t.Fatal("收件人无法读取自己的提及消息")
	}
}

func TestGroupMentionLoopHasBound(t *testing.T) {
	var firstName, secondName string
	var calls atomic.Int32
	r, first := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		target := secondName
		if !strings.Contains(req.System, "Name: "+firstName+"\n") {
			target = firstName
		}
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "@" + target + " 继续回应。"})
	}), DefaultLimits())
	group := createTestGroup(t, r, first, domain.DefaultGroupBudgetMicro)
	firstName = first.Name
	for _, member := range group.Members {
		if member.BotID != first.ID {
			secondName = member.Name
			break
		}
	}
	sent, err := r.Submit(context.Background(), group.ConversationID, SendInput{Content: "开始", Mentions: []string{first.ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := waitGroupTurn(t, r, sent.TurnID)
	if status != "succeeded" || calls.Load() != domain.MaxGroupMentionDepth+1 {
		t.Fatal("互相提及未有界结束", status, calls.Load())
	}
	var encoded string
	if err := r.deps.DB.QueryRow("SELECT plan_json FROM turns WHERE id=?", sent.TurnID).Scan(&encoded); err != nil || !strings.Contains(encoded, "GROUP_MENTION_LIMIT") {
		t.Fatal("截断未提供明确状态", encoded, err)
	}
}
