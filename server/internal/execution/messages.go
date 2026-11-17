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
// 用户单聊与群聊共用入口；猫咪私信只允许受租约保护的内建工具创建。
func (service *Service) Send(ctx context.Context, principal identity.Principal, conversationID string, request SendRequest) (Submission, error) {
	if !validID(conversationID) || !validID(request.ClientRequestID) || !validText(request.Content, 32<<10) || request.ReplyToID != "" && !validID(request.ReplyToID) {
		return Submission{}, ErrInvalidInput
	}
	var submitted Submission
	err := service.user(ctx, principal, true, func(transaction *sql.Tx) error {
		var kind string
		if err := transaction.QueryRowContext(ctx, `SELECT kind FROM tombot.conversations WHERE user_id=$1 AND id=$2`, principal.UserID, conversationID).Scan(&kind); err != nil {
			return err
		}
		if kind == "group" {
			return service.sendGroupInput(ctx, transaction, principal.UserID, conversationID, request, nil, &submitted)
		}
		if len(request.Mentions) > 0 || len(request.FileIDs) > 0 {
			return ErrInvalidInput
		}
		return service.submitInput(ctx, transaction, principal.UserID, conversationID, request, nil, "chat", &submitted)
	})
	if err != nil {
		return Submission{}, err
	}
	return submitted, nil
}

// submitInput 让正文与表情交互共用入队、补充输入和幂等事务，调用方不得在提交前发布事件。
func (service *Service) submitInput(ctx context.Context, transaction *sql.Tx, userID, conversationID string, request SendRequest, reaction *ReactionChange, source string, submitted *Submission) error {
	encoded, err := json.Marshal(struct {
		ConversationID string
		Content        string
		ReplyToID      string          `json:",omitempty"`
		Reaction       *ReactionChange `json:",omitempty"`
	}{conversationID, request.Content, request.ReplyToID, reaction})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	if err := lockDispatch(ctx, transaction, userID); err != nil {
		return err
	}
	var inputDigest []byte
	err = transaction.QueryRowContext(ctx, `SELECT turn_id,run_id,message_id,request_digest FROM tombot.run_inputs WHERE user_id=$1 AND client_request_id=$2`, userID, request.ClientRequestID).Scan(&submitted.TurnID, &submitted.RunID, &submitted.MessageID, &inputDigest)
	if err == nil {
		if !bytes.Equal(inputDigest, digest[:]) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var previous []byte
	err = transaction.QueryRowContext(ctx, `SELECT id,request_digest FROM tombot.turns WHERE user_id=$1 AND client_request_id=$2`, userID, request.ClientRequestID).Scan(&submitted.TurnID, &previous)
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
	botID, err := lockInputConversation(ctx, transaction, userID, conversationID, source == "routine")
	if err != nil {
		return err
	}
	reply, err := readReply(ctx, transaction, userID, conversationID, request.ReplyToID)
	if err != nil {
		return err
	}
	var activeRun, activeTurn string
	err = transaction.QueryRowContext(ctx, `SELECT r.id,r.turn_id FROM tombot.runs r JOIN tombot.turns t ON t.user_id=r.user_id AND t.id=r.turn_id
            WHERE r.user_id=$1 AND r.conversation_id=$2 AND t.status IN ('queued','running','canceling') AND r.status IN ('queued','running','canceling') ORDER BY r.id DESC LIMIT 1`, userID, conversationID).Scan(&activeRun, &activeTurn)
	if err == nil {
		if source == "routine" {
			return ErrConflict
		}
		return service.appendInput(ctx, transaction, userID, conversationID, activeTurn, activeRun, request, inputBlocks(request, reaction), reply, digest[:], submitted)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
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
			FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived`, userID, botID).Scan(&spec.Name, &spec.Description, &spec.AvatarRef, &spec.Category, &spec.SystemPrompt, &tools, &spec.ApprovalMode, &messaging, &spec.AccentHue, &revision)
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
	*submitted = Submission{TurnID: ids.New(), RunID: ids.New(), MessageID: ids.New(), IsCreated: true}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,$4,$5,$6,$7,'queued')`, userID, submitted.TurnID, conversationID, turnSequence, request.ClientRequestID, digest[:], source); err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status) VALUES($1,$2,$3,$4,$5,$6,$7,'queued')`, userID, submitted.RunID, submitted.TurnID, conversationID, botID, revision, snapshot); err != nil {
		return err
	}
	blocks, err := json.Marshal(inputBlocks(request, reaction))
	if err != nil {
		return err
	}
	senderKind, executionKind := "user", "chat"
	if source == "routine" {
		senderKind, executionKind = "system", "task"
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status,reply_to_id) VALUES($1,$2,$3,$4,$5,$8,$6,'complete',NULLIF($7,''))`, userID, submitted.MessageID, conversationID, submitted.TurnID, messageSequence, blocks, request.ReplyToID, senderKind); err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status,execution_kind) VALUES($1,$2,$3,$4,$5,$6,'queued',$7)`, userID, ids.New(), submitted.RunID, botID, conversationID, submitted.TurnID, executionKind); err != nil {
		return err
	}
	if err = event(ctx, transaction, userID, "message.created", submitted.MessageID, map[string]any{"conversation_id": conversationID, "sequence": messageSequence, "turn_id": submitted.TurnID}); err != nil {
		return err
	}
	return event(ctx, transaction, userID, "run.queued", submitted.RunID, map[string]string{"conversation_id": conversationID, "turn_id": submitted.TurnID, "bot_id": botID})
}

// lockDispatch 固定用户内受理顺序，使表情操作、消息发送与相同请求重试串行。
func lockDispatch(ctx context.Context, transaction *sql.Tx, userID string) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO tombot.user_dispatch_heads(user_id) VALUES($1) ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return err
	}
	var locked string
	return transaction.QueryRowContext(ctx, `SELECT user_id FROM tombot.user_dispatch_heads WHERE user_id=$1 FOR UPDATE`, userID).Scan(&locked)
}

// lockConversation 与执行收尾共享会话锁，拒绝已归档资源和尚未支持的会话类型。
func lockConversation(ctx context.Context, transaction *sql.Tx, userID, conversationID string) (string, error) {
	return lockInputConversation(ctx, transaction, userID, conversationID, false)
}
func lockInputConversation(ctx context.Context, transaction *sql.Tx, userID, conversationID string, background bool) (string, error) {
	var kind string
	var botID *string
	var archived bool
	if err := transaction.QueryRowContext(ctx, `SELECT c.kind,c.bot_id,c.is_archived OR COALESCE(b.is_archived,false) FROM tombot.conversations c LEFT JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id WHERE c.user_id=$1 AND c.id=$2 FOR UPDATE OF c`, userID, conversationID).Scan(&kind, &botID, &archived); err != nil {
		return "", err
	}
	if (!background && kind != "direct") || (background && kind != "task") {
		return "", ErrConversationKind
	}
	if archived || botID == nil {
		return "", ErrUnavailable
	}
	return *botID, nil
}

// inputBlocks 保留交互类型，避免把表情伪装成用户输入的普通正文。
func inputBlocks(request SendRequest, reaction *ReactionChange) []Block {
	if reaction != nil {
		return []Block{{Kind: "reaction", Reaction: reaction}}
	}
	return []Block{{Kind: "text", Text: request.Content}}
}

// 请求标识随历史返回，使事件先于发送回执到达时，客户端也能合并本地气泡。
// 首条输入的标识在回合上，补充输入在 run_inputs；关联使用既有索引，每页最多一百条。
const messageColumns = `messages.id,messages.conversation_id,messages.turn_id,messages.run_id,messages.sequence,messages.sender_kind,messages.sender_bot_id,messages.blocks,messages.status,messages.created_at,
CASE WHEN messages.sender_kind='user' THEN COALESCE(
    (SELECT i.client_request_id FROM tombot.run_inputs i WHERE i.user_id=messages.user_id AND i.turn_id=messages.turn_id AND i.message_id=messages.id),
    (SELECT t.client_request_id FROM tombot.turns t WHERE t.user_id=messages.user_id AND t.id=messages.turn_id),'') ELSE '' END,reply_target.id,reply_target.sender_kind,reply_target.blocks,messages.reaction_revision,
COALESCE((SELECT jsonb_agg(jsonb_build_object('emoji',r.emoji,'count',1) ORDER BY r.created_at,r.emoji) FROM tombot.message_reactions r WHERE r.user_id=messages.user_id AND r.message_id=messages.id),'[]'::jsonb)`

const messageSource = `tombot.messages LEFT JOIN tombot.messages reply_target ON reply_target.user_id=messages.user_id AND reply_target.conversation_id=messages.conversation_id AND reply_target.id=messages.reply_to_id`

func scanMessage(row scanner) (Message, error) {
	var message Message
	var blocks, replyBlocks, reactions []byte
	var replyID, replyKind *string
	err := row.Scan(&message.ID, &message.ConversationID, &message.TurnID, &message.RunID, &message.Sequence, &message.SenderKind, &message.SenderBotID, &blocks, &message.Status, &message.CreatedAt, &message.ClientRequestID, &replyID, &replyKind, &replyBlocks, &message.ReactionRevision, &reactions)
	if err != nil {
		return Message{}, err
	}
	if err = json.Unmarshal(blocks, &message.Blocks); err != nil {
		return Message{}, err
	}
	if err := json.Unmarshal(reactions, &message.Reactions); err != nil {
		return Message{}, err
	}
	if replyID != nil {
		message.ReplyTo = &MessageReply{MessageID: *replyID, senderKind: *replyKind}
		if err := decodeReply(message.ReplyTo, replyBlocks); err != nil {
			return Message{}, err
		}
	}
	return message, nil
}
func listMessages(ctx context.Context, transaction *sql.Tx, userID, conversationID string, before int64, limit int, modelBot string) (messages []Message, result error) {
	rows, err := transaction.QueryContext(ctx, `SELECT `+messageColumns+` FROM `+messageSource+` WHERE messages.user_id=$1 AND messages.conversation_id=$2 AND ($3::bigint=0 OR messages.sequence<$3) AND ($5<>'' OR NOT messages.blocks @> '[{"kind":"reaction"}]'::jsonb) AND (messages.visible_to_bot_id IS NULL OR messages.visible_to_bot_id=NULLIF($5,'')) ORDER BY messages.sequence DESC LIMIT $4`, userID, conversationID, before, limit, modelBot)
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
		messages, err = listMessages(ctx, transaction, principal.UserID, conversationID, before, limit, "")
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
