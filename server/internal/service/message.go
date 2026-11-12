package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
)

// BindReply 把回复指向同一会话里的另一条消息。空标识表示这条消息不是回复。
func BindReply(ctx context.Context, tx *sql.Tx, messageID, replyToID, conversationID string) error {
	if replyToID == "" {
		return nil
	}
	if replyToID == messageID {
		return domain.NewError(400, "MESSAGE_INVALID", "A message cannot reply to itself.")
	}
	var same int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=? AND conversation_id=?`, replyToID, conversationID).Scan(&same); err != nil {
		return err
	}
	if same != 1 {
		return domain.NewError(400, "MESSAGE_INVALID", "The reply target is not in this conversation.")
	}
	_, err := tx.ExecContext(ctx, `UPDATE messages SET reply_to_id=? WHERE id=?`, replyToID, messageID)
	return err
}

// AppendMessage 在调用方事务内分配会话序号并保存消息与事件，失败时由调用方整体回滚。
func AppendMessage(ctx context.Context, tx *sql.Tx, m *domain.Message) error {
	encoded, err := json.Marshal(m.Blocks)
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?", m.ConversationID).Scan(&m.Seq); err != nil {
		return err
	}
	audience, mentions := m.Audience, m.Mentions
	if audience == nil && m.RunID != nil {
		var raw string
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT audience_json FROM messages WHERE id=r.trigger_message_id),'[]') FROM runs r WHERE r.id=?", *m.RunID).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &audience); err != nil {
			return err
		}
	}
	if audience == nil {
		audience = []string{}
	}
	if mentions == nil {
		mentions = []string{}
	}
	encodedAudience, err := json.Marshal(audience)
	if err != nil {
		return err
	}
	encodedMentions, err := json.Marshal(mentions)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,turn_id,run_id,seq,sender_kind,sender_bot_id,blocks_json,status,token_in,token_out,created_at,audience_json,mentions_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.ConversationID, m.TurnID, m.RunID, m.Seq, m.SenderKind, m.SenderBotID, string(encoded), m.Status, m.TokenIn, m.TokenOut, m.CreatedAt.UnixMilli(), string(encodedAudience), string(encodedMentions)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE conversations SET updated_at=? WHERE id=?", m.CreatedAt.UnixMilli(), m.ConversationID); err != nil {
		return err
	}
	kind := "message.started"
	if m.Status != "streaming" {
		kind = "message.completed"
	}
	_, err = bus.Append(ctx, tx, kind, m.ConversationID, map[string]any{"run_id": m.RunID, "message_id": m.ID, "sender_bot_id": m.SenderBotID, "message": m})
	return err
}
