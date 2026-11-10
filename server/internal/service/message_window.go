package service

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/store"
)

// MessagesAround 返回包含指定序号的有限消息窗口；锚点必须属于当前会话，容量超限时明确失败。
func (s *Conversations) MessagesAround(ctx context.Context, id string, seq int64, limit int) ([]domain.Message, error) {
	if !validConversationID(id) || seq < 1 || limit < 1 || limit > 200 {
		return nil, badInput("Use a valid conversation, positive sequence and a limit from 1 to 200.")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	var messages []domain.Message
	err := store.Transact(ctx, s.db, func(tx *sql.Tx) error {
		var anchor int64
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM messages WHERE conversation_id=? AND seq=?", id, seq).Scan(&anchor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.NewError(404, "CONVERSATION_MESSAGE_NOT_FOUND", "The anchor message does not exist in this conversation.")
			}
			return err
		}
		var start int64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(seq) FROM (SELECT seq FROM messages WHERE conversation_id=? AND seq<=? ORDER BY seq DESC LIMIT ?)`, id, anchor, limit/2+1).Scan(&start); err != nil {
			return err
		}
		const window = ` FROM messages WHERE conversation_id=? AND seq>=? ORDER BY seq LIMIT ?`
		var size int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM (SELECT length(CAST(blocks_json AS BLOB)) AS size"+window+")", id, start, limit).Scan(&size); err != nil {
			return err
		}
		if size > maxTranscriptBytes {
			return domain.NewError(413, "MESSAGE_WINDOW_TOO_LARGE", "This message context exceeds one MiB. Read a smaller window.")
		}
		rows, err := tx.QueryContext(ctx, `SELECT * FROM (SELECT id,conversation_id,turn_id,run_id,seq,sender_kind,sender_bot_id,blocks_json,status,token_in,token_out,created_at`+window+`) ORDER BY seq DESC`, id, start, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		messages, err = readMessages(rows)
		if err = errors.Join(err, rows.Close()); err != nil {
			return err
		}
		return attachToolActivities(ctx, tx, messages, nil)
	})
	if err != nil {
		return nil, searchError(ctx, err)
	}
	return messages, nil
}
