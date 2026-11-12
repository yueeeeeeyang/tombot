package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func searchFixture(t *testing.T) (*Conversations, domain.Bot) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	bot, err := NewBots(db, events, t.TempDir()).Create(t.Context(), BotInput{Template: "researcher", Name: "资料检索员", RoleTitle: "历史核验负责人"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	return NewConversations(db, events), bot
}

func insertSearchMessage(t *testing.T, s *Conversations, conversation string, seq int64, blocks []domain.MessageBlock) string {
	t.Helper()
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	id := testkit.Random(t)
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES(?,?,?,'user',?,1000)`, id, conversation, seq, string(encoded)); err != nil {
		t.Fatal(err)
	}
	return id
}

func requireSearchCode(t *testing.T, err error, code string) {
	t.Helper()
	var fault *domain.Error
	if !errors.As(err, &fault) || fault.Code != code {
		t.Fatalf("错误码应为 %s，实际 %v", code, err)
	}
}

func TestGlobalSearchHistoricalMessagesAndPublicProjection(t *testing.T) {
	s, bot := searchFixture(t)
	ctx := t.Context()
	hitID := ""
	for seq := int64(1); seq <= 70; seq++ {
		text := "最新近况不含历史关键词"
		if seq == 3 {
			text = strings.Repeat("背景资料", 120) + "会议记录保存在这里。"
		}
		id := insertSearchMessage(t, s, bot.ConversationID, seq, []domain.MessageBlock{{Kind: "text", Text: text}})
		if seq == 3 {
			hitID = id
		}
	}
	page, err := s.Search(ctx, "会议记录", "", 20)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("未找到首屏以外的历史正文", page, err)
	}
	hit := page.Items[0]
	if hit.Kind != "message" || hit.MessageID != hitID || hit.ID != hitID || hit.Seq != 3 || hit.ConversationID != bot.ConversationID || hit.BotID == nil || *hit.BotID != bot.ID || hit.ConversationKind != "direct" || hit.Source != "user" || !strings.Contains(hit.Preview, "会议记录") || len([]rune(hit.Preview)) > 241 {
		t.Fatal("历史命中无法定位或摘要未围绕命中", hit)
	}
	for _, query := range []string{"资料检索员", "历史核验负责人"} {
		page, err = s.Search(ctx, query, "", 20)
		if err != nil || len(page.Items) == 0 || page.Items[0].Kind != "bot" || page.Items[0].ID != bot.ID || page.Items[0].Preview != bot.RoleTitle {
			t.Fatal("Bot 名称或职责未命中", query, page, err)
		}
	}
	privateMarker := "非公开材料标记"
	insertSearchMessage(t, s, bot.ConversationID, 71, []domain.MessageBlock{
		{Kind: "text", Text: "公开发言"},
		{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "call", Name: "file_read", Input: map[string]string{"path": privateMarker}}},
		{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "call", Content: privateMarker}},
		{Kind: "artifact", Artifact: &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: privateMarker}}},
	})
	if _, err := s.db.ExecContext(ctx, "UPDATE bots SET soul=? WHERE id=?", privateMarker, bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE conversations SET summary=? WHERE id=?", privateMarker, bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, privateMarker, "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("搜索泄露了角色提示词、摘要或工具私有字段", page, err)
	}
	page, err = s.Search(ctx, "公开发言", "", 20)
	encoded, marshalErr := json.Marshal(page)
	if err != nil || marshalErr != nil || len(page.Items) != 1 || strings.Contains(string(encoded), privateMarker) {
		t.Fatal("公开正文命中带出了其他消息块", err, marshalErr)
	}
	noteID := insertSearchMessage(t, s, bot.ConversationID, 72, []domain.MessageBlock{{Kind: "note", Text: "公开说明记录"}})
	page, err = s.Search(ctx, "说明", "", 20)
	if err != nil || len(page.Items) != 1 || page.Items[0].MessageID != noteID {
		t.Fatal("短中文词或公开说明未命中", page, err)
	}
	if _, err = s.db.ExecContext(ctx, "UPDATE messages SET status='streaming' WHERE id=?", noteID); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "说明", "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("流式未完成消息进入检索", page, err)
	}
}

func TestGlobalSearchPaginationTitleAndLiteralEscaping(t *testing.T) {
	s, bot := searchFixture(t)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, "UPDATE bots SET name='分页同事',role_title='分页职责',created_at=1000 WHERE id=?", bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE conversations SET title='分页会话',created_at=1000 WHERE id=?", bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 5; seq++ {
		insertSearchMessage(t, s, bot.ConversationID, seq, []domain.MessageBlock{{Kind: "text", Text: "分页正文"}})
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 7 {
			t.Fatal("分页没有结束")
		}
		page, err := s.Search(ctx, "分页", cursor, 1)
		if err != nil || len(page.Items) != 1 {
			t.Fatal("同时间分页丢失条目", page, err)
		}
		item := page.Items[0]
		key := item.Kind + ":" + item.ID
		if seen[key] {
			t.Fatal("同时间分页重复", key)
		}
		seen[key] = true
		if pages == 0 && item.Kind != "bot" || pages == 1 && (item.Kind != "conversation" || item.Title != "分页会话") || pages > 1 && item.Kind != "message" {
			t.Fatal("分类顺序不稳定", item)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatal("分页未遍历全部匹配", seen)
	}
	_, err := s.Search(ctx, "其他内容", cursor, 1)
	requireSearchCode(t, err, "SEARCH_CURSOR_INVALID")
	queries := []string{"%", "_", `\`, `"`, "100%_", `" OR *`, "a_b", "'; DROP TABLE messages;--"}
	for index, query := range queries {
		t.Run(fmt.Sprintf("字面词%d", index), func(t *testing.T) {
			id := insertSearchMessage(t, s, bot.ConversationID, int64(20+index), []domain.MessageBlock{{Kind: "text", Text: "前缀 " + query + " 后缀"}})
			page, err := s.Search(ctx, query, "", 50)
			if err != nil {
				t.Fatal("查询被解释为 SQL 或 FTS 语法", err)
			}
			found := false
			for _, item := range page.Items {
				found = found || item.MessageID == id
				if !strings.Contains(item.Preview, query) {
					t.Fatal("字面查询匹配了无关正文", query, item)
				}
			}
			if !found {
				t.Fatal("转义后无法检索原文", query)
			}
		})
	}
}

func TestGlobalSearchUpdatesDeletionArchiveAndGroupNavigation(t *testing.T) {
	s, bot := searchFixture(t)
	ctx := t.Context()
	group, err := NewGroups(s.db, s.events).Create(ctx, GroupInput{Name: "群聊标题核验", Members: []string{bot.ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	id := insertSearchMessage(t, s, group.ConversationID, 1, []domain.MessageBlock{{Kind: "text", Text: "旧版群聊正文"}})
	page, err := s.Search(ctx, "群聊", "", 20)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("群聊标题和历史正文未命中", page, err)
	}
	for _, item := range page.Items {
		if item.GroupID == nil || *item.GroupID != group.ID || item.ConversationKind != "group" || item.ConversationID != group.ConversationID || item.BotID != nil {
			t.Fatal("群聊结果无法导航", item)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET blocks_json='[{"kind":"text","text":"新版群聊正文"}]' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "旧版", "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("更新消息后旧索引未删除", page, err)
	}
	page, err = s.Search(ctx, "新版", "", 20)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("更新正文未索引", page, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM messages WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "新版", "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("删除消息后旧索引未删除", page, err)
	}
	insertSearchMessage(t, s, group.ConversationID, 2, []domain.MessageBlock{{Kind: "text", Text: "归档群聊正文"}})
	if _, err := s.db.ExecContext(ctx, "UPDATE conversations SET is_archived=1 WHERE id=?", group.ConversationID); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "群聊", "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("归档会话仍进入搜索", page, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE bots SET is_archived=1 WHERE id=?", bot.ID); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(ctx, "历史核验负责人", "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("归档 Bot 仍进入搜索", page, err)
	}
}

func TestGlobalSearchValidationCancellationAndDeadline(t *testing.T) {
	s, _ := searchFixture(t)
	for _, query := range []string{"", "   ", "a\x00b", "a\nb", string([]byte{0xff}), strings.Repeat("字", 129)} {
		_, err := s.Search(t.Context(), query, "", 20)
		requireSearchCode(t, err, "SEARCH_QUERY_INVALID")
	}
	for _, limit := range []int{0, -1, 51} {
		_, err := s.Search(t.Context(), "检索", "", limit)
		requireSearchCode(t, err, "INVALID_LIMIT")
	}
	for _, cursor := range []string{"!", "e30", strings.Repeat("x", 2049)} {
		_, err := s.Search(t.Context(), "检索", cursor, 20)
		requireSearchCode(t, err, "SEARCH_CURSOR_INVALID")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Search(ctx, "检索", "", 20); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的搜索仍执行", err)
	}
	ctx, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	_, err := s.Search(ctx, "检索", "", 20)
	requireSearchCode(t, err, "SEARCH_TIMEOUT")
}

func TestMessageWindowAnchorsBoundsAndCapacity(t *testing.T) {
	s, bot := searchFixture(t)
	for seq := int64(1); seq <= 70; seq++ {
		insertSearchMessage(t, s, bot.ConversationID, seq, []domain.MessageBlock{{Kind: "text", Text: fmt.Sprintf("历史第%d条", seq)}})
	}
	for _, test := range []struct {
		seq         int64
		limit       int
		first, last int64
	}{{3, 5, 1, 5}, {35, 5, 33, 37}, {1, 5, 1, 5}, {70, 5, 68, 70}, {35, 1, 35, 35}} {
		messages, err := s.MessagesAround(t.Context(), bot.ConversationID, test.seq, test.limit)
		if err != nil || len(messages) != int(test.last-test.first+1) || messages[0].Seq != test.first || messages[len(messages)-1].Seq != test.last {
			t.Fatal("锚点窗口或消息顺序错误", test, messages, err)
		}
	}
	for _, test := range []struct {
		id  string
		seq int64
	}{{"missing", 3}, {bot.ConversationID, 71}} {
		_, err := s.MessagesAround(t.Context(), test.id, test.seq, 5)
		requireSearchCode(t, err, "CONVERSATION_MESSAGE_NOT_FOUND")
	}
	for _, test := range []struct {
		seq   int64
		limit int
	}{{0, 5}, {-1, 5}, {3, 0}, {3, 201}} {
		_, err := s.MessagesAround(t.Context(), bot.ConversationID, test.seq, test.limit)
		requireSearchCode(t, err, "INVALID_INPUT")
	}
	insertSearchMessage(t, s, bot.ConversationID, 71, []domain.MessageBlock{{Kind: "text", Text: strings.Repeat("x", maxTranscriptBytes+1)}})
	_, err := s.MessagesAround(t.Context(), bot.ConversationID, 71, 1)
	requireSearchCode(t, err, "MESSAGE_WINDOW_TOO_LARGE")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.MessagesAround(ctx, bot.ConversationID, 3, 5); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的消息窗口仍执行", err)
	}
}
