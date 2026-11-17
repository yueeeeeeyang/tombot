package execution

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/identity"
)

// ConversationPreview 只返回侧栏所需的短摘要与序号，不加载完整消息历史。
type ConversationPreview struct {
	ConversationID string `json:"conversation_id"`
	Sequence       int64  `json:"sequence"`
	Content        string `json:"content"`
}

// ConversationPreviews 在同一用户事务内批量读取至多一百个会话；越权或失效会话整批拒绝。
func (service *Service) ConversationPreviews(ctx context.Context, principal identity.Principal, conversationIDs []string) ([]ConversationPreview, error) {
	if len(conversationIDs) == 0 || len(conversationIDs) > 100 {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]bool, len(conversationIDs))
	for _, id := range conversationIDs {
		if !validID(id) || seen[id] {
			return nil, ErrInvalidInput
		}
		seen[id] = true
	}
	previews := make([]ConversationPreview, 0, len(conversationIDs))
	err := service.user(ctx, principal, false, func(tx *sql.Tx) (result error) {
		// 现有 (user_id, conversation_id, sequence) 索引按会话倒序取一条，避免 N+1 往返及全历史排序。
		rows, err := tx.QueryContext(ctx, `SELECT c.id,COALESCE(m.sequence,0),COALESCE(m.blocks,'[]'::jsonb)
   FROM tombot.conversations c LEFT JOIN LATERAL (
    SELECT sequence,blocks FROM tombot.messages
    WHERE user_id=c.user_id AND conversation_id=c.id AND visible_to_bot_id IS NULL
     AND NOT blocks @> '[{"kind":"reaction"}]'::jsonb
    ORDER BY sequence DESC LIMIT 1
   ) m ON true WHERE c.user_id=$1 AND c.id=ANY($2::text[]) AND NOT c.is_archived AND c.kind IN ('direct','group')`, principal.UserID, conversationIDs)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, rows.Close()) }()
		for rows.Next() {
			var preview ConversationPreview
			var blocks []byte
			if err := rows.Scan(&preview.ConversationID, &preview.Sequence, &blocks); err != nil {
				return err
			}
			if preview.Sequence != 0 {
				var reply MessageReply
				if err := decodeReply(&reply, blocks); err != nil {
					return err
				}
				preview.Content = reply.Excerpt
			}
			previews = append(previews, preview)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(previews) != len(conversationIDs) {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return previews, nil
}
