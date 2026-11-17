package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestConversationPreviews(t *testing.T) {
	cluster := pgtest.Start(t)
	f := newFixture(t, cluster)
	ctx := context.Background()
	group, _, err := f.catalog.CreateGroup(ctx, f.a, bots.CreateGroupRequest{ClientRequestID: ids.New(), GroupSpec: bots.GroupSpec{Name: "摘要讨论组", MemberIDs: []string{f.first.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	request := SendRequest{ClientRequestID: ids.New(), Content: strings.Repeat("最新摘要", 30)}
	submitted, err := f.service.Send(ctx, f.a, f.first.ConversationID, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "群聊最后一条"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.SetReaction(ctx, f.a, submitted.MessageID, ReactionRequest{ClientRequestID: ids.New(), Emoji: "👍", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	previews, err := f.service.ConversationPreviews(ctx, f.a, []string{f.first.ConversationID, f.second.ConversationID, group.ConversationID})
	if err != nil || len(previews) != 3 {
		t.Fatal("摘要批量查询失败", previews, err)
	}
	for _, preview := range previews {
		switch preview.ConversationID {
		case f.first.ConversationID:
			if len([]rune(preview.Content)) != 80 || !strings.HasSuffix(preview.Content, "…") || !strings.HasPrefix(preview.Content, "最新摘要") {
				t.Fatal("正文未截断或混入内部表情事件", preview)
			}
		case f.second.ConversationID:
			if preview.Sequence != 0 || preview.Content != "" {
				t.Fatal("空会话被伪造消息", preview)
			}
		case group.ConversationID:
			if preview.Content != "群聊最后一条" {
				t.Fatal("群聊摘要错误", preview)
			}
		}
	}
	if _, err = f.service.ConversationPreviews(ctx, f.a, []string{f.first.ConversationID, f.foreign.ConversationID}); !errors.Is(err, ErrNotFound) {
		t.Fatal("跨用户会话没有整批拒绝", err)
	}
	for _, list := range [][]string{nil, {"invalid"}, {f.first.ConversationID, f.first.ConversationID}, make([]string, 101)} {
		if _, err = f.service.ConversationPreviews(ctx, f.a, list); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("非法查询未拒绝", err)
		}
	}
}
