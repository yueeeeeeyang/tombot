package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

// Send 在一个事务中保存消息、回合、运行及任务；返回受理不等于模型已经执行。
// 当前入口仅处理用户单聊，群聊与机器人私信须由对应编排入口创建任务。
func (service *Service) Send(ctx context.Context, principal identity.Principal, conversationID string, request SendRequest) (Submission, error) {
	if !validID(conversationID) || !validID(request.ClientRequestID) || !validText(request.Content, 32<<10) {
		return Submission{}, ErrInvalidInput
	}
	encoded, err := json.Marshal(struct{ ConversationID, Content string }{conversationID, request.Content})
	if err != nil {
		return Submission{}, err
	}
	digest := sha256.Sum256(encoded)
	var submitted Submission
	err = service.user(ctx, principal, true, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.user_dispatch_heads(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, principal.UserID); err != nil {
			return err
		}
		var userID string
		if err := transaction.QueryRowContext(ctx, `SELECT user_id FROM tombot.user_dispatch_heads WHERE user_id=$1 FOR UPDATE`, principal.UserID).Scan(&userID); err != nil {
			return err
		}
		var previous []byte
		err := transaction.QueryRowContext(ctx, `SELECT id,request_digest FROM tombot.turns WHERE user_id=$1 AND client_request_id=$2`, userID, request.ClientRequestID).Scan(&submitted.TurnID, &previous)
		if err == nil {
			if !bytes.Equal(previous, digest[:]) {
				return ErrConflict
			}
			return transaction.QueryRowContext(ctx, `SELECT r.id,m.id FROM tombot.runs r JOIN tombot.messages m ON m.user_id=r.user_id AND m.turn_id=r.turn_id AND m.sender_kind='user'
				WHERE r.user_id=$1 AND r.turn_id=$2 ORDER BY r.id,m.sequence LIMIT 1`, userID, submitted.TurnID).Scan(&submitted.RunID, &submitted.MessageID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var kind string
		var botID *string
		var archived bool
		if err = transaction.QueryRowContext(ctx, `SELECT kind,bot_id,is_archived FROM tombot.conversations WHERE user_id=$1 AND id=$2 FOR UPDATE`, userID, conversationID).Scan(&kind, &botID, &archived); err != nil {
			return err
		}
		if kind != "direct" {
			return ErrConversationKind
		}
		if archived || botID == nil {
			return ErrUnavailable
		}
		var busy bool
		if err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.turns WHERE user_id=$1 AND conversation_id=$2 AND status IN ('queued','running','canceling'))`, userID, conversationID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrBusy
		}
		var maximum, count int
		err = transaction.QueryRowContext(ctx, `SELECT queued_runs,(SELECT count(*) FROM tombot.jobs WHERE user_id=$1 AND status='queued') FROM tombot.user_limits WHERE user_id=$1`, userID).Scan(&maximum, &count)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if count >= maximum {
			return ErrCapacity
		}
		var spec bots.Spec
		var revision int64
		var tools, messaging []byte
		err = transaction.QueryRowContext(ctx, `SELECT name,description,avatar_ref,category,system_prompt,tool_permissions,approval_mode,messaging_policy,accent_hue,revision
			FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived`, userID, *botID).Scan(&spec.Name, &spec.Description, &spec.AvatarRef, &spec.Category, &spec.SystemPrompt, &tools, &spec.ApprovalMode, &messaging, &spec.AccentHue, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(tools, &spec.ToolPermissions); err != nil {
			return err
		}
		if err = json.Unmarshal(messaging, &spec.MessagingPolicy); err != nil {
			return err
		}
		snapshot, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		var turnSequence, messageSequence int64
		if err = transaction.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1,message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp()
			WHERE user_id=$1 AND id=$2 RETURNING turn_sequence,message_sequence`, userID, conversationID).Scan(&turnSequence, &messageSequence); err != nil {
			return err
		}
		submitted = Submission{TurnID: ids.New(), RunID: ids.New(), MessageID: ids.New(), IsCreated: true}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,$4,$5,$6,'chat','queued')`, userID, submitted.TurnID, conversationID, turnSequence, request.ClientRequestID, digest[:]); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status) VALUES($1,$2,$3,$4,$5,$6,$7,'queued')`, userID, submitted.RunID, submitted.TurnID, conversationID, *botID, revision, snapshot); err != nil {
			return err
		}
		blocks, err := json.Marshal([]Block{{Kind: "text", Text: request.Content}})
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,$5,'user',$6,'complete')`, userID, submitted.MessageID, conversationID, submitted.TurnID, messageSequence, blocks); err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, userID, ids.New(), submitted.RunID, *botID, conversationID, submitted.TurnID); err != nil {
			return err
		}
		if err = event(ctx, transaction, userID, "message.created", submitted.MessageID, map[string]any{"conversation_id": conversationID, "sequence": messageSequence, "turn_id": submitted.TurnID}); err != nil {
			return err
		}
		return event(ctx, transaction, userID, "run.queued", submitted.RunID, map[string]string{"conversation_id": conversationID, "turn_id": submitted.TurnID, "bot_id": *botID})
	})
	if err != nil {
		return Submission{}, err
	}
	return submitted, nil
}

const messageColumns = `id,conversation_id,turn_id,run_id,sequence,sender_kind,sender_bot_id,blocks,status,created_at`

func scanMessage(row scanner) (Message, error) {
	var message Message
	var blocks []byte
	err := row.Scan(&message.ID, &message.ConversationID, &message.TurnID, &message.RunID, &message.Sequence, &message.SenderKind, &message.SenderBotID, &blocks, &message.Status, &message.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	if err = json.Unmarshal(blocks, &message.Blocks); err != nil {
		return Message{}, err
	}
	return message, nil
}
func listMessages(ctx context.Context, transaction *sql.Tx, userID, conversationID string, before int64, limit int) (messages []Message, result error) {
	rows, err := transaction.QueryContext(ctx, `SELECT `+messageColumns+` FROM tombot.messages WHERE user_id=$1 AND conversation_id=$2 AND ($3::bigint=0 OR sequence<$3) ORDER BY sequence DESC LIMIT $4`, userID, conversationID, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	messages = make([]Message, 0, limit)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// Messages 按消息序号倒序分页；先验证会话存在，其他用户会话不返回空列表掩盖越权。
func (service *Service) Messages(ctx context.Context, principal identity.Principal, conversationID string, before int64, limit int) ([]Message, error) {
	if !validID(conversationID) || before < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	var messages []Message
	err := service.user(ctx, principal, false, func(transaction *sql.Tx) error {
		var id string
		if err := transaction.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID).Scan(&id); err != nil {
			return err
		}
		var err error
		messages, err = listMessages(ctx, transaction, principal.UserID, conversationID, before, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

// MarkRead 只推进本人会话已读位置，拒绝未来序号，不因旧设备回包而倒退。
func (service *Service) MarkRead(ctx context.Context, principal identity.Principal, conversationID string, sequence int64) error {
	if !validID(conversationID) || sequence < 0 {
		return ErrInvalidInput
	}
	return service.user(ctx, principal, true, func(transaction *sql.Tx) error {
		var current, maximum int64
		if err := transaction.QueryRowContext(ctx, `SELECT last_read_sequence,message_sequence FROM tombot.conversations WHERE user_id=$1 AND id=$2 FOR UPDATE`, principal.UserID, conversationID).Scan(&current, &maximum); err != nil {
			return err
		}
		if sequence > maximum {
			return ErrInvalidInput
		}
		if sequence <= current {
			return nil
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE tombot.conversations SET last_read_sequence=$3,revision=revision+1 WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID, sequence); err != nil {
			return err
		}
		return event(ctx, transaction, principal.UserID, "conversation.read", conversationID, map[string]int64{"sequence": sequence})
	})
}

// GetRun 读取当前用户的运行元数据，不返回机器人提示词或模型凭据。
func (service *Service) GetRun(ctx context.Context, principal identity.Principal, runID string) (Run, error) {
	if !validID(runID) {
		return Run{}, ErrInvalidInput
	}
	var run Run
	err := service.user(ctx, principal, false, func(transaction *sql.Tx) error {
		var err error
		run, err = readRun(ctx, transaction, principal.UserID, runID)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}
