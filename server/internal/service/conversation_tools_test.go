package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

func appendTranscript(t *testing.T, s *Conversations, conversationID, text string) domain.Message {
	t.Helper()
	message := domain.Message{ID: ids.New(), ConversationID: conversationID, SenderKind: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: text}}, Status: "complete", CreatedAt: time.Now().UTC()}
	if err := store.Transact(context.Background(), s.db, func(tx *sql.Tx) error { return AppendMessage(context.Background(), tx, &message) }); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestConversationSearchAccessFiltersShortTermsAndPrivatePayloads(t *testing.T) {
	groups, bots, members := groupFixture(t)
	ctx := context.Background()
	s := NewConversations(bots.db, groups.events)
	group, err := groups.Create(ctx, GroupInput{Name: "共享资料", Members: []string{members[0].ID, members[1].ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	dmID, routineID := ids.New(), ids.New()
	if _, err = bots.db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,initiator_bot_id,title,created_at,updated_at) VALUES(?,'bot_dm','bot_message',?,?,'成员私信',0,0)`, dmID, members[1].ID, members[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err = bots.db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES(?,'direct','routine',?,'例程资料',0,0)`, routineID, members[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{members[0].ConversationID, members[1].ConversationID, members[2].ConversationID, group.ConversationID, dmID, routineID} {
		appendTranscript(t, s, id, "公开分析 abc SQL 与中文检索")
	}
	for _, query := range []string{"中文检索", "分析", "SQL", "ab", "abc"} {
		hits, err := s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: query})
		if err != nil || len(hits) != 4 {
			t.Fatal("访问范围或短查询错误", query, len(hits), err)
		}
		for _, hit := range hits {
			if hit.ConversationID == members[1].ConversationID || hit.ConversationID == members[2].ConversationID {
				t.Fatal("检索越权")
			}
		}
	}
	hits, err := s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "分析", BotID: members[1].ID})
	if err != nil || len(hits) != 2 {
		t.Fatal("参与者筛选改变调用身份", hits, err)
	}
	hits, err = s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "分析", Kind: "group"})
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	hits, err = s.SearchTranscript(ctx, members[2].ID, domain.ConversationSearch{Query: "分析", BotID: members[0].ID})
	if err != nil || len(hits) != 0 {
		t.Fatal("借用他人身份检索", hits, err)
	}
	private := domain.Message{ID: ids.New(), ConversationID: group.ConversationID, SenderKind: "bot", SenderBotID: &members[1].ID, Status: "complete", CreatedAt: time.Now(), Blocks: []domain.MessageBlock{{Kind: "text", Text: "对外发言"}, {Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "private", Content: "内部记忆内容"}}}}
	if err = store.Transact(ctx, s.db, func(tx *sql.Tx) error { return AppendMessage(ctx, tx, &private) }); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"内部记忆", "tool_result", "kind", "分析 OR hidden"} {
		hits, err = s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: query})
		if err != nil || len(hits) != 0 {
			t.Fatal("技术字段、工具结果或 FTS 操作符泄漏", query, hits, err)
		}
	}
	transcript, err := s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: group.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(transcript)
	if err != nil || strings.Contains(string(encoded), "内部记忆内容") {
		t.Fatal("上下文泄漏私有工具结果", err)
	}
	if _, err = bots.db.Exec("DELETE FROM group_members WHERE group_id=? AND bot_id=?", group.ID, members[0].ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: group.ConversationID})
	groupError(t, err, "CONVERSATION_FORBIDDEN")
	hits, err = s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "对外发言"})
	if err != nil || len(hits) != 0 {
		t.Fatal("移出群后仍可检索", hits, err)
	}
	for _, input := range []domain.ConversationSearch{{Query: ""}, {Query: "\x00"}, {Query: "abc", Kind: "other"}, {Query: "abc", Limit: 51}, {Query: strings.Repeat("abc ", 17)}} {
		if _, err = s.SearchTranscript(ctx, members[0].ID, input); err == nil {
			t.Fatal("无效搜索被接受", input)
		}
	}
}

func TestConversationReadWindowIndexUpdatesAndFailureRollback(t *testing.T) {
	groups, bots, members := groupFixture(t)
	ctx := context.Background()
	s := NewConversations(bots.db, groups.events)
	messages := []domain.Message{}
	for range 7 {
		messages = append(messages, appendTranscript(t, s, members[0].ConversationID, "原始正文分析"))
	}
	latest, err := s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, Limit: 3})
	if err != nil || len(latest.Messages) != 3 || latest.Messages[0].ID != messages[4].ID || !latest.HasBefore || latest.HasAfter {
		t.Fatal(latest, err)
	}
	around, err := s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, AroundMessageID: messages[3].ID, Limit: 3})
	if err != nil || len(around.Messages) != 3 || around.Messages[0].ID != messages[2].ID || !around.HasAfter || !around.HasBefore {
		t.Fatal(around, err)
	}
	foreign := appendTranscript(t, s, members[1].ConversationID, "其他会话")
	_, err = s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, AroundMessageID: foreign.ID})
	groupError(t, err, "CONVERSATION_MESSAGE_NOT_FOUND")
	for _, id := range []string{members[1].ConversationID, "missing"} {
		_, err = s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: id})
		groupError(t, err, "CONVERSATION_FORBIDDEN")
	}
	if _, err = bots.db.Exec(`UPDATE messages SET blocks_json='[{"kind":"text","text":"新版发言"}]' WHERE id=?`, messages[0].ID); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "新版发言"})
	if err != nil || len(hits) != 1 {
		t.Fatal("索引更新丢失", hits, err)
	}
	if _, err = bots.db.Exec("DELETE FROM messages WHERE id=?", messages[0].ID); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "新版发言"})
	if err != nil || len(hits) != 0 {
		t.Fatal("删除未同步索引", hits, err)
	}
	large := appendTranscript(t, s, members[0].ConversationID, strings.Repeat("正文", 100000))
	appendTranscript(t, s, members[0].ConversationID, strings.Repeat("正文", 100000))
	_, err = s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, Limit: 2})
	groupError(t, err, "CONVERSATION_TOO_LARGE")
	one, err := s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, AroundMessageID: large.ID, Limit: 1})
	if err != nil || len(one.Messages) != 1 || len(one.Messages[0].Text) != 600000 {
		t.Fatal("小窗口正文被截断", len(one.Messages), err)
	}
	hits, err = s.SearchTranscript(ctx, members[0].ID, domain.ConversationSearch{Query: "分析"})
	if err != nil || len(hits) == 0 {
		t.Fatal("检索失败", hits, err)
	}
	_, err = s.ReadTranscript(ctx, members[0].ID, domain.ConversationRead{ConversationID: members[0].ConversationID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
}
