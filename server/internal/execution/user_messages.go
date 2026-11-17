package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/infra/ids"
)

// writeDirectMessage 在调用方事务及用户受理锁内投递，不创建模型运行，也不占用聊天执行槽。
// 接收者只能是当前租户内该猫咪自己的主聊天；来源和发言标记均由服务端生成。
func writeDirectMessage(ctx context.Context, tx *sql.Tx, user, bot string, blocks []Block) (string, error) {
	var conversation string
	err := tx.QueryRowContext(ctx, `SELECT c.id FROM tombot.conversations c JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id WHERE c.user_id=$1 AND c.bot_id=$2 AND c.kind='direct' AND NOT c.is_archived AND NOT b.is_archived FOR UPDATE OF c`, user, bot).Scan(&conversation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnavailable
	}
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return "", err
	}
	var turnSequence, sequence int64
	if err = tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1,message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING turn_sequence,message_sequence`, user, conversation).Scan(&turnSequence, &sequence); err != nil {
		return "", err
	}
	turn, message := ids.New(), ids.New()
	digest := sha256.Sum256(encoded)
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status,finished_at) VALUES($1,$2,$3,$4,$5,$6,'proactive','succeeded',clock_timestamp())`, user, turn, conversation, turnSequence, ids.New(), digest[:]); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,$5,'system',$6,'complete')`, user, message, conversation, turn, sequence, encoded); err != nil {
		return "", err
	}
	return message, event(ctx, tx, user, "message.created", message, map[string]any{"bot_id": bot, "conversation_id": conversation, "sequence": sequence, "turn_id": turn})
}

// writePrivateNotices 复用客户端既有私信入口；正文只在同租户的私信窗口读取，不进入主聊天模型上下文。
func writePrivateNotices(ctx context.Context, tx *sql.Tx, user, sender, recipient, conversation string) error {
	var senderName, recipientName string
	if err := tx.QueryRowContext(ctx, `SELECT a.name,b.name FROM tombot.bots a JOIN tombot.bots b ON b.user_id=a.user_id WHERE a.user_id=$1 AND a.id=$2 AND b.id=$3`, user, sender, recipient).Scan(&senderName, &recipientName); err != nil {
		return err
	}
	for _, item := range []struct{ bot, direction, peer, name, text string }{
		{sender, "sent", recipient, recipientName, "已发消息给" + recipientName},
		{recipient, "received", sender, senderName, "收到" + senderName + "的消息"},
	} {
		if _, err := writeDirectMessage(ctx, tx, user, item.bot, []Block{{Kind: "dm_notice", Text: item.text, DMNotice: &botmessaging.Notice{Direction: item.direction, PeerID: item.peer, PeerName: item.name, ConversationID: conversation}}}); err != nil {
			return err
		}
	}
	return nil
}
