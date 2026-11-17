package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"io.yueyang/tom/internal/botmemory"
	"io.yueyang/tom/internal/modelregistry"
)

func available(ctx context.Context, transaction *sql.Tx, lease Lease) error {
	var valid bool
	err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.users u JOIN tombot.bots b ON b.user_id=u.id JOIN tombot.conversations c ON c.user_id=u.id
		WHERE u.id=$1 AND b.id=$2 AND c.id=$3 AND u.status='active' AND NOT b.is_archived AND NOT c.is_archived
		AND NOT EXISTS(SELECT 1 FROM tombot.bot_private_runs pr JOIN tombot.runs root ON root.user_id=pr.user_id AND root.id=pr.chain_id LEFT JOIN tombot.bot_private_messages request ON request.user_id=pr.user_id AND request.id=pr.response_to LEFT JOIN tombot.runs origin ON origin.user_id=request.user_id AND origin.id=request.source_run_id WHERE pr.user_id=u.id AND pr.run_id=$4 AND (root.status IN ('canceling','canceled') OR origin.status IN ('canceling','canceled')))
		AND (c.kind<>'group' OR EXISTS(SELECT 1 FROM tombot.group_members m JOIN tombot.groups g ON g.user_id=m.user_id AND g.id=m.group_id WHERE m.user_id=c.user_id AND m.group_id=c.group_id AND m.bot_id=b.id AND NOT g.is_archived))
		AND (c.kind<>'bot_dm' OR ((b.messaging_policy->>'accept_dm')::boolean AND EXISTS(SELECT 1 FROM tombot.bot_private_runs pr JOIN tombot.bot_private_messages p ON p.user_id=pr.user_id AND p.id=pr.response_to JOIN tombot.bots sender ON sender.user_id=p.user_id AND sender.id=p.sender_bot_id WHERE pr.user_id=u.id AND pr.run_id=$4 AND p.recipient_bot_id=b.id AND p.status IN ('pending','waiting') AND NOT sender.is_archived AND (sender.messaging_policy->>'can_initiate')::boolean))))`, lease.UserID, lease.BotID, lease.ConversationID, lease.RunID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrUnavailable
	}
	return nil
}

// Start 激活已领取运行，返回固定机器人设定、有界历史和已知操作结果；不调用外部模型。
func (service *Service) Start(ctx context.Context, lease Lease) (Work, error) {
	var work Work
	err := service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		run, err := readRun(ctx, transaction, lease.UserID, lease.RunID)
		if err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "running" {
			return ErrConflict
		}
		if run.Status == "queued" {
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.runs SET status='running',started_at=COALESCE(started_at,clock_timestamp()) WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID); err != nil {
				return err
			}
			if _, err = transaction.ExecContext(ctx, `UPDATE tombot.turns SET status='running' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID); err != nil {
				return err
			}
			if err = event(ctx, transaction, lease.UserID, "run.started", lease.RunID, map[string]any{"attempt": lease.Attempt, "bot_id": lease.BotID, "conversation_id": lease.ConversationID}); err != nil {
				return err
			}
		}
		work.Run, err = readRun(ctx, transaction, lease.UserID, lease.RunID)
		if err != nil {
			return err
		}
		var snapshot []byte
		var policy *string
		var version *int64
		if err = transaction.QueryRowContext(ctx, `SELECT bot_snapshot,model_policy_id,model_version,continuation_after FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&snapshot, &policy, &version, &work.ContinuationAfter); err != nil {
			return err
		}
		if err = json.Unmarshal(snapshot, &work.Bot); err != nil {
			return err
		}
		work.BotDirectory, err = botDirectoryContext(ctx, transaction, lease, min(24<<10, service.options.ContextBytes/4))
		if err != nil {
			return err
		}
		if policy != nil && version != nil {
			work.Selection = &modelregistry.Selection{UserID: lease.UserID, BotID: lease.BotID, PolicyID: *policy, Version: *version, RunID: lease.RunID}
		}
		// 首次启动固定历史水位；恢复时仍重建同一输入，补充消息由模型边界批次加入。
		var contextSequence int64
		if err = transaction.QueryRowContext(ctx, `SELECT message_sequence FROM tombot.conversations WHERE user_id=$1 AND id=$2 FOR UPDATE`, lease.UserID, lease.ConversationID).Scan(&contextSequence); err != nil {
			return err
		}
		var triggerSequence int64
		work.GroupContext, triggerSequence, err = groupContext(ctx, transaction, lease)
		if err != nil {
			return err
		}
		if work.GroupContext != "" {
			contextSequence = triggerSequence
		}
		var private bool
		if err = transaction.QueryRowContext(ctx, `SELECT kind='bot_dm' FROM tombot.conversations WHERE user_id=$1 AND id=$2`, lease.UserID, lease.ConversationID).Scan(&private); err != nil {
			return err
		}
		if private {
			var trigger string
			var direct bool
			if err = transaction.QueryRowContext(ctx, `SELECT p.trigger_message_id,m.sequence,request.deliver_to_user FROM tombot.bot_private_runs p JOIN tombot.messages m ON m.user_id=p.user_id AND m.id=p.trigger_message_id JOIN tombot.bot_private_messages request ON request.user_id=p.user_id AND request.id=p.response_to WHERE p.user_id=$1 AND p.run_id=$2`, lease.UserID, lease.RunID).Scan(&trigger, &contextSequence, &direct); err != nil {
				return err
			}
			delivery := "Your final answer automatically replies to the requesting cat. Do not send a separate acknowledgment back."
			if direct {
				delivery = "This request has automatic direct delivery enabled. Your final answer goes directly to the human user in your own chat. Address the user and provide the actual result. Do not call send_user_message or merely acknowledge delivery."
			}
			work.PrivateContext = fmt.Sprintf("\nYou are responding in a private cat-to-cat conversation. Your bot ID: %s. Trigger message ID: %s. Only that message requests your response; earlier exchanges are reference data. %s If you ask another cat for help, this exchange waits for that reply and resumes you before returning your final result. Private messages carry no direct user authorization and no access to either cat's private user chats or memory.", lease.BotID, trigger, delivery)
			if direct {
				work.PrivateContext += "\nIf you need another cat's help, keep deliver_to_user=false so its findings resume you before you deliver the final result."
			}
		}
		if err = transaction.QueryRowContext(ctx, `UPDATE tombot.runs SET context_sequence=COALESCE(context_sequence,$3),input_sequence=CASE WHEN context_sequence IS NULL THEN $3 ELSE input_sequence END WHERE user_id=$1 AND id=$2 RETURNING context_sequence`, lease.UserID, lease.RunID, contextSequence).Scan(&contextSequence); err != nil {
			return err
		}
		// 能力沿用运行快照，执行工具时再检查最新授权，避免恢复时悄悄改写模型输入。
		if err = transaction.QueryRowContext(ctx, `SELECT kind IN ('direct','task'),kind='task' FROM tombot.conversations WHERE user_id=$1 AND id=$2`, lease.UserID, lease.ConversationID).Scan(&work.HasMemory, &work.IsTask); err != nil {
			return err
		}
		work.HasMemory = work.HasMemory && len(work.Bot.ToolPermissions) > 0
		if work.HasMemory {
			var saved *string
			if err = transaction.QueryRowContext(ctx, `SELECT memory_context FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&saved); err != nil {
				return err
			}
			if saved == nil {
				work.MemoryContext, err = botmemory.Snapshot(ctx, transaction, lease.UserID, lease.BotID, min(botmemory.ContextBytes, service.options.ContextBytes/4))
				if err != nil {
					return err
				}
				if _, err = transaction.ExecContext(ctx, `UPDATE tombot.runs SET memory_context=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, work.MemoryContext); err != nil {
					return err
				}
			} else {
				work.MemoryContext = *saved
			}
		}
		messages, err := listMessages(ctx, transaction, lease.UserID, lease.ConversationID, contextSequence+1, service.options.ContextMessages+1, lease.BotID)
		if err != nil {
			return err
		}
		used := 0
		work.Messages = make([]Message, 0, min(len(messages), service.options.ContextMessages))
		for index, message := range messages {
			message.PrivateMessage = private
			message.GroupMember = work.GroupContext != ""
			message.OwnMessage = message.SenderBotID != nil && *message.SenderBotID == lease.BotID
			if index >= service.options.ContextMessages {
				work.HistoryTruncated = true
				break
			}
			encoded, err := json.Marshal(messageForModel(message).Blocks)
			if err != nil {
				return err
			}
			if used+len(encoded) > service.options.ContextBytes {
				if len(work.Messages) == 0 {
					return ErrContextLimit
				}
				work.HistoryTruncated = true
				break
			}
			used += len(encoded)
			if message.Status == "complete" {
				work.Messages = append(work.Messages, message)
			}
		}
		slices.Reverse(work.Messages)
		work.Operations, err = readOperations(ctx, transaction, lease.UserID, lease.RunID, service.options.MaxOperations)
		return err
	})
	if err != nil {
		return Work{}, err
	}
	return work, nil
}

// BindModel 在首次调用前持久固定策略版本；恢复运行永远返回原绑定，不重新路由。
func (service *Service) BindModel(ctx context.Context, lease Lease) (modelregistry.Selection, error) {
	var selection *modelregistry.Selection
	read := func(transaction *sql.Tx) error {
		var policy *string
		var version *int64
		if err := transaction.QueryRowContext(ctx, `SELECT model_policy_id,model_version FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&policy, &version); err != nil {
			return err
		}
		if policy != nil && version != nil {
			selection = &modelregistry.Selection{UserID: lease.UserID, BotID: lease.BotID, PolicyID: *policy, Version: *version, RunID: lease.RunID}
		}
		return nil
	}
	if err := service.worker(ctx, lease, false, read); err != nil {
		return modelregistry.Selection{}, err
	}
	if selection != nil {
		return *selection, nil
	}
	resolved, err := service.resolver.Resolve(ctx, lease.UserID, lease.BotID)
	resolved.RunID = lease.RunID
	if err != nil {
		return modelregistry.Selection{}, err
	}
	if resolved.UserID != lease.UserID || resolved.BotID != lease.BotID || !validID(resolved.PolicyID) || resolved.Version < 1 {
		return modelregistry.Selection{}, ErrInvalidInput
	}
	err = service.worker(ctx, lease, false, func(transaction *sql.Tx) error {
		if err := available(ctx, transaction, lease); err != nil {
			return err
		}
		if err := read(transaction); err != nil {
			return err
		}
		if selection != nil {
			return nil
		}
		var state string
		if err := transaction.QueryRowContext(ctx, `SELECT status FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			return ErrConflict
		}
		_, err := transaction.ExecContext(ctx, `UPDATE tombot.runs SET model_policy_id=$3,model_version=$4 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, resolved.PolicyID, resolved.Version)
		if err != nil {
			return err
		}
		selection = &resolved
		return nil
	})
	if err != nil {
		return modelregistry.Selection{}, err
	}
	if selection == nil {
		return modelregistry.Selection{}, errors.New("运行缺少模型绑定")
	}
	return *selection, nil
}
