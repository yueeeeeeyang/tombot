package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

// SendUserMessage 校验发言者与当前工具调用，用户不使用 Bot 标识，也不生成子任务。
func (m runMessenger) SendUserMessage(ctx context.Context, run domain.Run, callID, content string) (domain.BotMessageReply, error) {
	if run.ID != m.parent.run.ID || run.BotID != m.parent.run.BotID {
		return domain.BotMessageReply{}, domain.NewError(403, "BOT_MESSAGE_CONTEXT", "Only the current bot may send this message.")
	}
	content = strings.TrimSpace(content)
	if err := service.ValidateUserMessage(content); err != nil {
		return domain.BotMessageReply{}, err
	}
	return m.runtime.publishUserMessage(ctx, run, callID, []domain.MessageBlock{{Kind: "text", Text: content}}, true)
}

// publishUserMessage 幂等写入发送猫咪自己的用户会话，保留真实运行归属并立即通知桌面。
func (r *Runtime) publishUserMessage(ctx context.Context, run domain.Run, id string, blocks []domain.MessageBlock, tool bool) (reply domain.BotMessageReply, err error) {
	var content strings.Builder
	for _, block := range blocks {
		if block.Kind == "text" {
			content.WriteString(block.Text)
			content.WriteString("\n")
		}
	}
	if text := strings.TrimSpace(content.String()); text != "" || len(blocks) == 0 {
		if err := service.ValidateUserMessage(text); err != nil {
			return reply, err
		}
	}
	reply = domain.BotMessageReply{RunID: run.ID, Artifacts: []string{}}
	err = store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=? AND bot_id=? AND status='running')`, run.ID, run.BotID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return domain.NewError(409, "BOT_MESSAGE_CONTEXT", "User messages require the active sending bot's run.")
		}
		if tool {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tool_calls WHERE id=? AND run_id=? AND tool_name='message_user' AND status='running')`, id, run.ID).Scan(&active); err != nil {
				return err
			}
			if !active {
				return domain.NewError(403, "BOT_MESSAGE_CONTEXT", "An active message_user tool call is required.")
			}
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE bot_id=? AND kind='direct' AND source!='routine' AND is_archived=0`, run.BotID).Scan(&reply.ConversationID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=? AND conversation_id=? AND sender_bot_id=?)`, id, reply.ConversationID, run.BotID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		message := domain.Message{ID: id, ConversationID: reply.ConversationID, RunID: &run.ID, SenderKind: "bot", SenderBotID: &run.BotID, Blocks: blocks, Status: "complete", CreatedAt: time.Now().UTC(), Audience: []string{}}
		if err := service.AppendMessage(ctx, tx, &message); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, "bot:"+run.BotID, "message.user", id, map[string]string{"run_id": run.ID, "bot_id": run.BotID, "conversation_id": reply.ConversationID}); err != nil {
			return err
		}
		if _, err := bus.Append(ctx, tx, "user.message", reply.ConversationID, map[string]string{"message_id": id, "bot_id": run.BotID, "run_id": run.ID}); err != nil {
			return err
		}
		return service.AppendConversationEvent(ctx, tx, reply.ConversationID)
	})
	if err == nil {
		reply.Delivered = true
		reply.DeliveredToUser = true
		r.deps.Events.Notify()
	}
	return
}

// deliverUserResult 在接收猫咪确实成功执行后投递它的原始结果；不让发送者代写或重新总结。
func (r *Runtime) deliverUserResult(ctx context.Context, j *job) error {
	var posted bool
	if err := r.deps.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.run_id=? AND c.kind='direct' AND c.bot_id=? AND m.sender_bot_id=?)`, j.run.ID, j.run.BotID, j.run.BotID).Scan(&posted); err != nil {
		return err
	}
	if posted {
		return nil
	}
	var encoded string
	if err := r.deps.DB.QueryRowContext(ctx, `SELECT blocks_json FROM messages WHERE run_id=? AND conversation_id=? AND sender_kind='bot' AND status='complete' ORDER BY seq DESC LIMIT 1`, j.run.ID, j.run.ConversationID).Scan(&encoded); err != nil {
		return err
	}
	var blocks []domain.MessageBlock
	if err := json.Unmarshal([]byte(encoded), &blocks); err != nil {
		return err
	}
	visible := []domain.MessageBlock{}
	for _, block := range blocks {
		if block.Kind == "text" && strings.TrimSpace(block.Text) != "" {
			visible = append(visible, block)
		} else if block.Kind == "artifact" {
			visible = append(visible, block)
		}
	}
	if len(visible) == 0 {
		return domain.NewError(502, "BOT_NO_REPLY", "The recipient did not produce a result to deliver to the user.")
	}
	_, err := r.publishUserMessage(ctx, j.run, "user-"+j.run.ID, visible, false)
	return err
}
