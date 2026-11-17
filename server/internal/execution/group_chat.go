package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/infra/ids"
)

// groupMembers 只读取当前群公开成员，不加载其他猫咪的提示词或长期记忆。
func groupMembers(ctx context.Context, tx *sql.Tx, user, conversation string) ([]groupchat.Member, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.id,b.name FROM tombot.conversations c JOIN tombot.groups g ON g.user_id=c.user_id AND g.id=c.group_id JOIN tombot.group_members m ON m.user_id=g.user_id AND m.group_id=g.id JOIN tombot.bots b ON b.user_id=m.user_id AND b.id=m.bot_id WHERE c.user_id=$1 AND c.id=$2 AND c.kind='group' AND NOT c.is_archived AND NOT g.is_archived AND NOT b.is_archived ORDER BY b.id LIMIT 33`, user, conversation)
	if err != nil {
		return nil, err
	}
	members := []groupchat.Member{}
	for rows.Next() {
		var member groupchat.Member
		if err = rows.Scan(&member.ID, &member.Name); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		members = append(members, member)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(members) == 0 || len(members) > groupchat.MaxMembers {
		return nil, ErrUnavailable
	}
	return members, nil
}

func groupReplyBot(ctx context.Context, tx *sql.Tx, user, conversation, reply string) (string, error) {
	if reply == "" {
		return "", nil
	}
	if _, err := readReply(ctx, tx, user, conversation, reply); err != nil {
		return "", err
	}
	var bot string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(sender_bot_id,'') FROM tombot.messages WHERE user_id=$1 AND conversation_id=$2 AND id=$3`, user, conversation, reply).Scan(&bot)
	return bot, err
}

// sendGroupInput 每次发送单独持久受理，不把新消息补入其他成员尚未结束的回合。
func (service *Service) sendGroupInput(ctx context.Context, tx *sql.Tx, user, conversation string, request SendRequest, reaction *ReactionChange, submitted *Submission) error {
	if err := lockDispatch(ctx, tx, user); err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		Conversation string
		Request      SendRequest
		Reaction     *ReactionChange
	}{conversation, request, reaction})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT t.id,t.request_digest,m.id,COALESCE((SELECT id FROM tombot.runs WHERE user_id=t.user_id AND turn_id=t.id ORDER BY id LIMIT 1),'') FROM tombot.turns t JOIN tombot.messages m ON m.user_id=t.user_id AND m.turn_id=t.id AND m.sender_kind='user' WHERE t.user_id=$1 AND t.client_request_id=$2`, user, request.ClientRequestID).Scan(&submitted.TurnID, &previous, &submitted.MessageID, &submitted.RunID)
	if err == nil {
		if !bytes.Equal(previous, digest[:]) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var used bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.run_inputs WHERE user_id=$1 AND client_request_id=$2)`, user, request.ClientRequestID).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrConflict
	}
	var locked string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND id=$2 AND kind='group' AND NOT is_archived FOR UPDATE`, user, conversation).Scan(&locked); err != nil {
		return err
	}
	members, err := groupMembers(ctx, tx, user, conversation)
	if err != nil {
		return err
	}
	mentions, err := groupchat.Mentions(request.Content, members, request.Mentions)
	if err != nil {
		return err
	}
	replyBot, err := groupReplyBot(ctx, tx, user, conversation, request.ReplyToID)
	if err != nil {
		return err
	}
	if replyBot != "" && len(mentions) == 0 && !slices.ContainsFunc(members, func(member groupchat.Member) bool { return member.ID == replyBot }) {
		return groupchat.ErrMention
	}
	reply, err := readReply(ctx, tx, user, conversation, request.ReplyToID)
	if err != nil {
		return err
	}
	attachments, err := groupFileBlocks(ctx, tx, user, conversation, request.FileIDs)
	if err != nil {
		return err
	}
	blocks := append(inputBlocks(request, reaction), attachments...)
	contextBlocks, err := json.Marshal(messageForModel(Message{SenderKind: "user", Blocks: blocks, ReplyTo: reply}).Blocks)
	if err != nil {
		return err
	}
	if len(contextBlocks) > service.options.ContextBytes {
		return ErrContextLimit
	}
	recipients := groupchat.Recipients(members, mentions, replyBot, "user")
	if reaction != nil && replyBot == "" {
		recipients = nil
	}
	if err = groupCapacity(ctx, tx, user, len(recipients)); err != nil {
		return err
	}
	*submitted = Submission{TurnID: ids.New(), MessageID: ids.New(), IsCreated: true}
	var turnSequence int64
	if err = tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1 WHERE user_id=$1 AND id=$2 RETURNING turn_sequence`, user, conversation).Scan(&turnSequence); err != nil {
		return err
	}
	status := "queued"
	if len(recipients) == 0 {
		status = "succeeded"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status,finished_at) VALUES($1,$2,$3,$4,$5,$6,'group',$7,CASE WHEN $7='succeeded' THEN clock_timestamp() END)`, user, submitted.TurnID, conversation, turnSequence, request.ClientRequestID, digest[:], status); err != nil {
		return err
	}
	blocks[0].Mentions = mentions
	if err = writeGroupMessage(ctx, tx, user, conversation, submitted.TurnID, "", "", submitted.MessageID, request.ReplyToID, blocks, "complete"); err != nil {
		return err
	}
	runs, err := enqueueGroup(ctx, tx, user, conversation, submitted.TurnID, submitted.MessageID, 0, members, recipients)
	if err != nil {
		return err
	}
	if len(runs) > 0 {
		submitted.RunID = runs[0]
	}
	return nil
}

func groupCapacity(ctx context.Context, tx *sql.Tx, user string, requested int) error {
	var maximum, current int
	err := tx.QueryRowContext(ctx, `SELECT queued_runs,(SELECT count(*) FROM tombot.jobs WHERE user_id=$1 AND status='queued') FROM tombot.user_limits WHERE user_id=$1`, user).Scan(&maximum, &current)
	if err != nil {
		return err
	}
	if current+requested > maximum {
		return ErrCapacity
	}
	return nil
}

// enqueueGroup 由持有群会话锁及用户受理锁的调用方使用；唯一投递键阻止恢复后的重复唤醒。
func enqueueGroup(ctx context.Context, tx *sql.Tx, user, conversation, turn, trigger string, depth int, members []groupchat.Member, recipients []string) ([]string, error) {
	roster, err := json.Marshal(members)
	if err != nil {
		return nil, err
	}
	runs := []string{}
	for _, bot := range recipients {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.group_deliveries WHERE user_id=$1 AND trigger_message_id=$2 AND bot_id=$3)`, user, trigger, bot).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		run := ids.New()
		result, err := tx.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status)
 SELECT user_id,$3,$4,$5,id,revision,jsonb_build_object('name',name,'description',description,'avatar_ref',avatar_ref,'category',category,'system_prompt',system_prompt,'tool_permissions',tool_permissions,'approval_mode',approval_mode,'messaging_policy',messaging_policy,'accent_hue',accent_hue),'queued' FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived`, user, bot, run, turn, conversation)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, ErrUnavailable
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.group_deliveries(user_id,run_id,bot_id,conversation_id,trigger_message_id,depth,roster) VALUES($1,$2,$3,$4,$5,$6,$7)`, user, run, bot, conversation, trigger, depth, roster); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, user, ids.New(), run, bot, conversation, turn); err != nil {
			return nil, err
		}
		if err = event(ctx, tx, user, "run.queued", run, map[string]string{"conversation_id": conversation, "bot_id": bot, "turn_id": turn}); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func writeGroupMessage(ctx context.Context, tx *sql.Tx, user, conversation, turn, run, bot, message, reply string, blocks []Block, status string) error {
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING message_sequence`, user, conversation).Scan(&sequence); err != nil {
		return err
	}
	kind := "user"
	if bot != "" {
		kind = "bot"
	} else if run != "" {
		kind = "system"
		run = ""
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,run_id,sender_bot_id,sequence,sender_kind,blocks,status,reply_to_id) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10,NULLIF($11,''))`, user, message, conversation, turn, run, bot, sequence, kind, encoded, status, reply); err != nil {
		return err
	}
	return event(ctx, tx, user, "message.created", message, map[string]any{"conversation_id": conversation, "sequence": sequence, "turn_id": turn})
}

// groupContext 固定公开成员和触发消息水位，恢复时不读取之后的新消息或私人记忆。
func groupContext(ctx context.Context, tx *sql.Tx, lease Lease) (string, int64, error) {
	var roster []byte
	var trigger string
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT d.roster,d.trigger_message_id,m.sequence FROM tombot.group_deliveries d JOIN tombot.messages m ON m.user_id=d.user_id AND m.id=d.trigger_message_id WHERE d.user_id=$1 AND d.run_id=$2`, lease.UserID, lease.RunID).Scan(&roster, &trigger, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return groupchat.Instructions + fmt.Sprintf("\nYour bot ID: %s\nTrigger message ID: %s\nPublic group roster (reference data): %s\nUser mention ID: user", lease.BotID, trigger, roster), sequence, nil
}
