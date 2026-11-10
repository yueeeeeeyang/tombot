package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
)

// AppendMessage 在调用方事务内分配会话序号并保存消息与事件，失败时由调用方整体回滚。
func AppendMessage(ctx context.Context, tx *sql.Tx, m *domain.Message) error {
	encoded, err := json.Marshal(m.Blocks)
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?", m.ConversationID).Scan(&m.Seq); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,turn_id,run_id,seq,sender_kind,sender_bot_id,blocks_json,status,token_in,token_out,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.ConversationID, m.TurnID, m.RunID, m.Seq, m.SenderKind, m.SenderBotID, string(encoded), m.Status, m.TokenIn, m.TokenOut, m.CreatedAt.UnixMilli()); err != nil {
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
