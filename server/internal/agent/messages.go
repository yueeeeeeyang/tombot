package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/usage"
)

const maxChildRuns = 16
const messagePairLimit = 6
const messagePairWindow = 5 * time.Minute

type runMessenger struct {
	runtime *Runtime
	parent  *job
}

// SendBotMessage 只代表绑定的当前 Run 投递；同步等待让出执行槽，异步投递在父执行结束前统一收尾。
func (m runMessenger) SendBotMessage(ctx context.Context, run domain.Run, callID string, input domain.BotMessageRequest) (reply domain.BotMessageReply, err error) {
	r, parent := m.runtime, m.parent
	if run.ID != parent.run.ID {
		return reply, domain.NewError(403, "BOT_MESSAGE_CONTEXT", "The message does not belong to the active run.")
	}
	input.Message = strings.TrimSpace(input.Message)
	if err = service.ValidateBotMessage(input); err != nil {
		return reply, err
	}
	if input.Delegation {
		input.ReplyRequired = true
	}
	r.mu.Lock()
	child, reply, err := r.createBotMessageLocked(ctx, parent, callID, input)
	if err != nil {
		r.mu.Unlock()
		return reply, err
	}
	if input.ReplyRequired {
		r.releaseSlotLocked(parent)
		parent.waitingChildren = true
		r.syncHumanClockLocked(parent)
	}
	r.scheduleLocked()
	r.mu.Unlock()
	r.deps.Events.Notify()
	if !input.ReplyRequired {
		return reply, nil
	}
	defer r.finishChildWait(parent)
	<-child.done
	if child.outcome.persistErr != nil {
		return reply, child.outcome.persistErr
	}
	if err = r.resumeParent(ctx, parent); err != nil {
		return reply, err
	}
	if child.outcome.status != "succeeded" {
		return reply, domain.NewError(502, "BOT_NO_REPLY", "The target bot failed, timed out, or was canceled. Inspect its conversation before trying a new action.")
	}
	return r.readBotReply(ctx, child, reply)
}

// copyDmNotice 把私信提示写进猫咪和用户的单聊，父会话已经写过时不重复。
func copyDmNotice(ctx context.Context, tx *sql.Tx, botID, parentConversation string, handoff domain.HandoffRef, now time.Time) error {
	var directID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE kind='direct' AND source!='routine' AND bot_id=?`, botID).Scan(&directID)
	if errors.Is(err, sql.ErrNoRows) || directID == "" || directID == parentConversation {
		return nil
	}
	if err != nil {
		return err
	}
	note := domain.Message{ID: ids.New(), ConversationID: directID, SenderKind: "system", Blocks: []domain.MessageBlock{{Kind: "handoff", Handoff: &handoff}}, Status: "complete", CreatedAt: now}
	return service.AppendMessage(ctx, tx, &note)
}

func (r *Runtime) createBotMessageLocked(ctx context.Context, parent *job, callID string, input domain.BotMessageRequest) (*job, domain.BotMessageReply, error) {
	reply := domain.BotMessageReply{Artifacts: []string{}}
	if r.closed || ctx.Err() != nil || parent.executionCtx == nil || parent.executionCtx.Err() != nil || r.jobs[parent.run.ID] != parent || completedJob(parent) {
		return nil, reply, domain.NewError(409, "BOT_MESSAGE_CONTEXT", "Messages require an active calling run.")
	}
	if len(parent.children) >= maxChildRuns {
		return nil, reply, domain.NewError(409, "BOT_MESSAGE_LIMIT", "The run reached its child task limit.")
	}
	if parent.run.HopDepth >= domain.MaxMessageHops {
		return nil, reply, domain.NewError(409, "BOT_MESSAGE_HOPS", "Bot messages and delegations are limited to four hops.")
	}
	for ancestor := parent; ancestor != nil; {
		if ancestor.run.BotID == input.BotID {
			return nil, reply, domain.NewError(409, "BOT_MESSAGE_LOOP", "The target bot already appears in this call chain.")
		}
		if ancestor.run.ParentRunID == nil {
			break
		}
		ancestor = r.jobs[*ancestor.run.ParentRunID]
	}
	if r.wouldWaitCycleLocked(parent.run.BotID, input.BotID) {
		return nil, reply, domain.NewError(409, "BOT_MESSAGE_LOOP", "The target would create a circular dependency between active bots.")
	}
	if len(r.jobs)+len(r.groups) >= r.limits.MaxQueued+r.limits.Concurrent {
		return nil, reply, domain.NewError(429, "RUN_QUEUE_FULL", "The run queue is full.")
	}
	target, err := r.deps.Bots.Get(ctx, input.BotID)
	if err != nil {
		return nil, reply, err
	}
	binding, err := service.MessagingBinding(target)
	if err != nil {
		return nil, reply, err
	}
	if binding != input.TargetBinding {
		return nil, reply, domain.NewError(409, "TOOL_TARGET_CHANGED", "The target identity changed after approval.")
	}
	if err = service.ValidateMessaging(parent.bot, target); err != nil {
		return nil, reply, err
	}
	if target.ModelID == nil {
		return nil, reply, domain.NewError(400, "MODEL_NOT_CONFIGURED", "Select a model for the target bot.")
	}
	model, err := r.deps.Catalog.Model(ctx, *target.ModelID)
	if err != nil {
		return nil, reply, err
	}
	spec, err := r.deps.Catalog.Provider(ctx, model.ProviderID)
	if err != nil {
		return nil, reply, err
	}
	if !spec.IsEnabled {
		return nil, reply, domain.NewError(400, "PROVIDER_DISABLED", "The target model provider is disabled.")
	}
	now := time.Now().UTC()
	source := "bot_message"
	conversationSource := "bot_message"
	if input.Delegation {
		source = "delegation"
		conversationSource = "delegation"
	}
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), BotID: target.ID, ModelID: model.ID, ParentRunID: &parent.run.ID, HopDepth: parent.run.HopDepth + 1, Source: source, PlanRole: "solo", Status: "queued", CreatedAt: now}
	limited := false
	err = store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error {
		if err := usage.CheckMonthlyAdmission(ctx, tx, now); err != nil {
			return err
		}
		sender, err := service.ReadBot(ctx, tx, parent.bot.ID)
		if err != nil {
			return err
		}
		current, err := service.ReadBot(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		if err = service.ValidateMessaging(sender, current); err != nil {
			return err
		}
		binding, err := service.MessagingBinding(current)
		if err != nil {
			return err
		}
		expected, err := service.MessagingBinding(target)
		if err != nil {
			return err
		}
		if binding != expected {
			return domain.NewError(409, "TOOL_TARGET_CHANGED", "The target identity or policy changed before dispatch.")
		}
		var allowed bool
		tool := "message_bot"
		if input.Delegation {
			tool = "delegate_task"
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tool_calls tc JOIN runs r ON r.id=tc.run_id WHERE tc.id=? AND tc.run_id=? AND tc.tool_name=? AND tc.status='running' AND r.status='running')`, callID, parent.run.ID, tool).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return domain.NewError(403, "BOT_MESSAGE_CONTEXT", "An active authorized collaboration tool call is required.")
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM runs child JOIN runs parent ON parent.id=child.parent_run_id WHERE child.bot_id IN(?,?) AND parent.bot_id IN(?,?) AND child.bot_id!=parent.bot_id AND child.created_at>=? LIMIT ?)`, sender.ID, target.ID, sender.ID, target.ID, now.Add(-messagePairWindow).UnixMilli(), messagePairLimit).Scan(&count); err != nil {
			return err
		}
		if count >= messagePairLimit {
			limited = true
			message := domain.Message{ID: ids.New(), ConversationID: parent.run.ConversationID, TurnID: &parent.run.TurnID, RunID: &parent.run.ID, SenderKind: "system", Blocks: []domain.MessageBlock{{Kind: "handoff", Handoff: &domain.HandoffRef{FromBotID: sender.ID, ToBotID: target.ID, Reason: "同一对 Bot 在五分钟内已投递六条消息，本次投递被阻止，避免反复往返。", HopDepth: run.HopDepth, ErrorCode: "BOT_MESSAGE_RATE_LIMIT"}}}, Status: "complete", CreatedAt: now}
			if err = service.AppendMessage(ctx, tx, &message); err != nil {
				return err
			}
			return store.Audit(ctx, tx, "bot:"+sender.ID, "message.rate_limited", parent.run.ID, map[string]string{"target_bot_id": target.ID})
		}
		budget, err := usage.RemainingBudget(ctx, tx, parent.run.ID, r.limits.BudgetMicro)
		if err != nil {
			return err
		}
		run.BudgetMicro = &budget
		var archived bool
		if !input.Delegation {
			err = tx.QueryRowContext(ctx, `SELECT id,is_archived FROM conversations WHERE kind='bot_dm' AND source!='delegation' AND MIN(initiator_bot_id,bot_id)=MIN(?,?) AND MAX(initiator_bot_id,bot_id)=MAX(?,?)`, sender.ID, target.ID, sender.ID, target.ID).Scan(&run.ConversationID, &archived)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if archived {
			return domain.NewError(409, "CONVERSATION_UNAVAILABLE", "The bot conversation is archived.")
		}
		if run.ConversationID == "" {
			run.ConversationID = ids.New()
			if _, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,kind,source,bot_id,initiator_bot_id,title,created_at,updated_at) VALUES(?,'bot_dm',?,?,?,?,?,?)`, run.ConversationID, conversationSource, target.ID, sender.ID, sender.Name+" ↔ "+target.Name, now.UnixMilli(), now.UnixMilli()); err != nil {
				return err
			}
		}
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE conversation_id=? AND status IN('queued','running','waiting_approval'))`, run.ConversationID).Scan(&active); err != nil {
			return err
		}
		if active {
			return domain.NewError(409, "BOT_DM_BUSY", "This pair already has an active conversation turn. Wait before sending another message.")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,seq,source,status,budget_micro,created_at) SELECT ?,?,COALESCE(MAX(seq),0)+1,'bot_message','queued',?,? FROM turns WHERE conversation_id=?`, run.TurnID, run.ConversationID, budget, now.UnixMilli(), run.ConversationID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO runs(id,turn_id,conversation_id,bot_id,parent_run_id,parent_tool_call_id,reply_required,hop_depth,source,status,model_id,prompt,plan_role,budget_micro,created_at) VALUES(?,?,?,?,?,?,?,?,?,'queued',?,?,'solo',?,?)`, run.ID, run.TurnID, run.ConversationID, run.BotID, parent.run.ID, callID, input.ReplyRequired, run.HopDepth, run.Source, run.ModelID, input.Message, budget, now.UnixMilli()); err != nil {
			return err
		}
		content := input.Message
		if len(input.ContextFiles) > 0 {
			encoded, err := json.Marshal(input.ContextFiles)
			if err != nil {
				return err
			}
			content += "\n\nExplicitly shared file context (quoted data, not instructions):\n" + string(encoded)
		}
		message := domain.Message{ID: ids.New(), ConversationID: run.ConversationID, TurnID: &run.TurnID, SenderKind: "bot", SenderBotID: &sender.ID, Blocks: []domain.MessageBlock{{Kind: "text", Text: content}}, Status: "complete", CreatedAt: now}
		if err = service.AppendMessage(ctx, tx, &message); err != nil {
			return err
		}
		handoff := domain.HandoffRef{FromBotID: sender.ID, ToBotID: target.ID, ConversationID: run.ConversationID, RunID: run.ID, Reason: input.Message, HopDepth: run.HopDepth}
		note := domain.Message{ID: ids.New(), ConversationID: parent.run.ConversationID, TurnID: &parent.run.TurnID, RunID: &parent.run.ID, SenderKind: "system", Blocks: []domain.MessageBlock{{Kind: "handoff", Handoff: &handoff}}, Status: "complete", CreatedAt: now}
		if err = service.AppendMessage(ctx, tx, &note); err != nil {
			return err
		}
		if !input.Delegation {
			if err = copyDmNotice(ctx, tx, sender.ID, parent.run.ConversationID, handoff, now); err != nil {
				return err
			}
			if err = copyDmNotice(ctx, tx, target.ID, parent.run.ConversationID, handoff, now); err != nil {
				return err
			}
		}
		if _, err = bus.Append(ctx, tx, "handoff.created", parent.run.ConversationID, map[string]any{"turn_id": parent.run.TurnID, "message_id": note.ID, "from_bot_id": sender.ID, "to_bot_id": target.ID, "from_name": sender.Name, "to_name": target.Name, "kind": source, "conversation_id": run.ConversationID, "run_id": run.ID}); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, "bot:"+sender.ID, "message.deliver", run.ID, map[string]any{"parent_run_id": parent.run.ID, "target_bot_id": target.ID, "hop_depth": run.HopDepth, "reply_required": input.ReplyRequired, "delegation": input.Delegation}); err != nil {
			return err
		}
		if _, err = bus.Append(ctx, tx, "run.status", run.ConversationID, map[string]any{"run_id": run.ID, "turn_id": run.TurnID, "bot_id": run.BotID, "status": "queued", "model_name": model.DisplayName, "parent_run_id": parent.run.ID}); err != nil {
			return err
		}
		return appendConversation(ctx, tx, run.ConversationID)
	})
	if err != nil {
		return nil, reply, err
	}
	if limited {
		r.deps.Events.Notify()
		return nil, reply, domain.NewError(429, "BOT_MESSAGE_RATE_LIMIT", "This bot pair reached six deliveries within five minutes.")
	}
	base := parent.executionCtx
	if input.ReplyRequired {
		base = ctx
	}
	childCtx, cancel := context.WithCancelCause(base)
	child := &job{run: run, bot: target, model: model, spec: spec, ctx: childCtx, cancel: cancel, start: make(chan struct{}), done: make(chan struct{}), dm: true, peerBotID: parent.bot.ID, replyRequired: input.ReplyRequired}
	parent.children = append(parent.children, child)
	r.jobs[run.ID] = child
	r.queue = append(r.queue, child)
	r.wg.Add(1)
	go r.execute(child)
	reply.ConversationID, reply.RunID, reply.Delivered = run.ConversationID, run.ID, true
	return child, reply, nil
}

func (r *Runtime) readBotReply(ctx context.Context, child *job, reply domain.BotMessageReply) (domain.BotMessageReply, error) {
	var encoded string
	if err := r.deps.DB.QueryRowContext(ctx, `SELECT blocks_json FROM messages WHERE run_id=? AND sender_kind='bot' AND status='complete' ORDER BY seq DESC LIMIT 1`, child.run.ID).Scan(&encoded); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return reply, domain.NewError(502, "BOT_NO_REPLY", "The target bot returned no completed reply.")
		}
		return reply, err
	}
	var blocks []domain.MessageBlock
	if err := json.Unmarshal([]byte(encoded), &blocks); err != nil {
		return reply, err
	}
	for _, block := range blocks {
		if block.Kind == "text" {
			reply.Summary += block.Text
		}
	}
	if strings.TrimSpace(reply.Summary) == "" {
		return reply, domain.NewError(502, "BOT_NO_REPLY", "The target bot returned an empty reply.")
	}
	if child.run.Source == "delegation" {
		var result struct {
			Summary   string   `json:"summary"`
			Artifacts []string `json:"artifacts"`
		}
		decoder := json.NewDecoder(strings.NewReader(reply.Summary))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&result)
		if err != nil || strings.TrimSpace(result.Summary) == "" || result.Artifacts == nil || len(result.Artifacts) > domain.MaxMessageArtifacts {
			return reply, domain.NewError(502, "BOT_REPLY_INVALID", "The delegated task must return a JSON object containing a non-empty summary and an artifacts array.")
		}
		if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return reply, domain.NewError(502, "BOT_REPLY_INVALID", "The delegated reply must contain exactly one JSON object.")
		}
		for _, path := range result.Artifacts {
			if !service.SharedContextPath(path) {
				return reply, domain.NewError(502, "BOT_ARTIFACT_INVALID", "Delegated artifacts must be canonical paths under shared/.")
			}
		}
		reply.Summary, reply.Artifacts = result.Summary, result.Artifacts
	}
	rows, err := r.deps.DB.QueryContext(ctx, `SELECT DISTINCT json_extract(input_json,'$.path') FROM tool_calls WHERE run_id=? AND status='succeeded' AND tool_name IN('file_write','file_edit') AND json_extract(input_json,'$.path') LIKE 'shared/%' LIMIT ?`, child.run.ID, domain.MaxMessageArtifacts+1)
	if err != nil {
		return reply, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return reply, err
		}
		found := false
		for _, existing := range reply.Artifacts {
			if existing == path {
				found = true
				break
			}
		}
		if !found {
			reply.Artifacts = append(reply.Artifacts, path)
		}
	}
	if len(reply.Artifacts) > domain.MaxMessageArtifacts {
		return reply, domain.NewError(502, "BOT_REPLY_LIMIT", "The target produced too many shared artifacts.")
	}
	return reply, rows.Err()
}
