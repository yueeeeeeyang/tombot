package service

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
)

func readMemberGroup(ctx context.Context, tx *sql.Tx, groupID, botID string) (domain.Group, error) {
	var member bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_members m JOIN bots b ON b.id=m.bot_id WHERE m.group_id=? AND m.bot_id=? AND b.is_archived=0)`, groupID, botID).Scan(&member); err != nil {
		return domain.Group{}, err
	}
	if !member {
		return domain.Group{}, domain.NewError(403, "GROUP_ACCESS_DENIED", "Only active members may access this group.")
	}
	return ReadGroup(ctx, tx, groupID)
}

// MemberGroup 只向当前成员提供群配置和公开成员资料，不读取其他成员的提示词或私有记忆。
func (s *Groups) MemberGroup(ctx context.Context, groupID, botID string) (group domain.Group, err error) {
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var readErr error
		group, readErr = readMemberGroup(ctx, tx, groupID, botID)
		return readErr
	})
	return
}

// PostMessage 在当前回合追加发言并保存提及；运行时在成员完成后有界投递，不在工具中等待其他猫咪。
func (s *Groups) PostMessage(ctx context.Context, groupID, callID string, run domain.Run, content string, mentions ...string) (message domain.Message, err error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > domain.MaxGroupPostBytes {
		return message, badInput("A group post must contain 1 to 16384 UTF-8 bytes.")
	}
	err = store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		group, err := readMemberGroup(ctx, tx, groupID, run.BotID)
		if err != nil {
			return err
		}
		if len(mentions) == 0 {
			mentions, err = domain.ResolveGroupMentions(content, group.Members)
			if err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		for _, id := range mentions {
			valid := id == "user"
			for _, member := range group.Members {
				valid = valid || (member.BotID == id && !member.IsArchived)
			}
			if !valid || seen[id] {
				return domain.NewError(400, "INVALID_GROUP_MENTION", "Mention unique active members or user.")
			}
			seen[id] = true
		}
		var allowed bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs r JOIN turns t ON t.id=r.turn_id JOIN conversations c ON c.id=r.conversation_id JOIN tool_calls tc ON tc.run_id=r.id WHERE r.id=? AND r.bot_id=? AND r.turn_id=? AND r.conversation_id=? AND c.kind='group' AND c.group_id=? AND r.source IN('group','routine') AND r.status='running' AND t.status IN('running','waiting_approval') AND tc.id=? AND tc.tool_name='post_to_group' AND tc.status='running')`, run.ID, run.BotID, run.TurnID, run.ConversationID, group.ID, callID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return domain.NewError(403, "GROUP_POST_CONTEXT", "Posting is only allowed from an active member run in the current group turn.")
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE run_id=? AND sender_kind='bot'", run.ID).Scan(&count); err != nil {
			return err
		}
		if count >= domain.MaxGroupRunMessages {
			return domain.NewError(409, "GROUP_POST_LIMIT", "The member run reached its public message limit.")
		}
		// 工具调用 ID 同时作为消息 ID，持久化重试也无法重复发布同一条公开发言。
		message = domain.Message{ID: callID, ConversationID: run.ConversationID, TurnID: &run.TurnID, RunID: &run.ID, SenderKind: "bot", Mentions: mentions, SenderBotID: &run.BotID, Blocks: []domain.MessageBlock{{Kind: "text", Text: content}}, Status: "complete", CreatedAt: time.Now().UTC()}
		// 指定收件人时只发布给这些成员和发送者；未指定则继承触发消息的可见范围。
		if len(mentions) > 0 {
			message.Audience = slices.Clone(mentions)
			message.Audience = append(message.Audience, run.BotID)
		}
		if err = AppendMessage(ctx, tx, &message); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, "bot:"+run.BotID, "group.post", callID, map[string]any{"group_id": group.ID, "run_id": run.ID, "message_id": message.ID, "content_bytes": len(content)}); err != nil {
			return err
		}
		return AppendConversationEvent(ctx, tx, run.ConversationID)
	})
	if err == nil {
		s.events.Notify()
	}
	return
}
