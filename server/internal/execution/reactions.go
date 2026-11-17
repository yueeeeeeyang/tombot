package execution

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"slices"

	"io.yueyang/tom/internal/identity"
)

// 前后端读取同一份静态名单，避免界面可选的表情被服务拒绝。
//
//go:embed reaction_emojis.json
var reactionEmojisJSON string

// SetReaction 设置明确目标状态；标记、去重回执与模型输入同一事务提交，失败全部回滚。
// 重试返回当前状态而不重放旧变更；无变化的请求不触发模型执行。
func (service *Service) SetReaction(ctx context.Context, principal identity.Principal, messageID string, request ReactionRequest) (Message, error) {
	var emojis []string
	if err := json.Unmarshal([]byte(reactionEmojisJSON), &emojis); err != nil {
		return Message{}, err
	}
	if !validID(messageID) || !validID(request.ClientRequestID) || !slices.Contains(emojis, request.Emoji) {
		return Message{}, ErrInvalidInput
	}
	var message Message
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		if err := lockDispatch(ctx, tx, principal.UserID); err != nil {
			return err
		}
		var conversationID string
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM tombot.messages WHERE user_id=$1 AND id=$2 AND `+interactiveMessage+` AND status='complete' AND blocks @> '[{"kind":"text"}]'::jsonb`, principal.UserID, messageID).Scan(&conversationID); err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID).Scan(&kind); err != nil {
			return err
		}
		botID := ""
		var err error
		if kind != "group" {
			botID, err = lockConversation(ctx, tx, principal.UserID, conversationID)
			if err != nil {
				return err
			}
		}
		var previousMessageID, previousEmoji string
		var previousActive bool
		err = tx.QueryRowContext(ctx, `SELECT message_id,emoji,is_active FROM tombot.message_reaction_requests WHERE user_id=$1 AND client_request_id=$2`, principal.UserID, request.ClientRequestID).Scan(&previousMessageID, &previousEmoji, &previousActive)
		if err == nil {
			if previousMessageID != messageID || previousEmoji != request.Emoji || previousActive != request.IsActive {
				return ErrConflict
			}
		} else {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.message_reaction_requests(user_id,client_request_id,message_id,emoji,is_active) VALUES($1,$2,$3,$4,$5)`, principal.UserID, request.ClientRequestID, messageID, request.Emoji, request.IsActive); err != nil {
				return err
			}
			var change sql.Result
			if request.IsActive {
				change, err = tx.ExecContext(ctx, `INSERT INTO tombot.message_reactions(user_id,message_id,emoji) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, principal.UserID, messageID, request.Emoji)
			} else {
				change, err = tx.ExecContext(ctx, `DELETE FROM tombot.message_reactions WHERE user_id=$1 AND message_id=$2 AND emoji=$3`, principal.UserID, messageID, request.Emoji)
			}
			if err != nil {
				return err
			}
			count, err := change.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				var submitted Submission
				input := SendRequest{ClientRequestID: request.ClientRequestID, ReplyToID: messageID}
				reaction := &ReactionChange{Emoji: request.Emoji, IsActive: request.IsActive}
				if kind == "group" {
					err = service.sendGroupInput(ctx, tx, principal.UserID, conversationID, input, reaction, &submitted)
				} else {
					err = service.submitInput(ctx, tx, principal.UserID, conversationID, input, reaction, "chat", &submitted)
				}
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE tombot.messages SET reaction_revision=reaction_revision+1 WHERE user_id=$1 AND id=$2`, principal.UserID, messageID); err != nil {
					return err
				}
				if err := event(ctx, tx, principal.UserID, "message.reactions_changed", messageID, map[string]string{"conversation_id": conversationID, "bot_id": botID}); err != nil {
					return err
				}
			}
		}
		message, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM `+messageSource+` WHERE messages.user_id=$1 AND messages.id=$2`, principal.UserID, messageID))
		return err
	})
	if err != nil {
		return Message{}, err
	}
	return message, nil
}
