package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
)

type botMessageTool struct {
	executor *Service
	schema   provider.ToolSchema
	maximum  int
}

func (tool botMessageTool) Schema() provider.ToolSchema { return tool.schema }
func (botMessageTool) UsesComputer() bool               { return false }
func (botMessageTool) RequiresVision() bool             { return false }

type privateRejection string

func (reason privateRejection) Error() string { return string(reason) }

// Execute 以现有租约保护目录读取与私信受理；成功、拒绝及选择次数都与工具回执原子保存。
func (tool botMessageTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		operation, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, invocation.OperationID))
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(invocation.Call)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		if invocation.Call.Name != tool.schema.Name || operation.Kind != "tool" || operation.Status != "started" || operation.FencingToken != lease.FencingToken || operation.Attempt != lease.Attempt || !bytes.Equal(digest[:], operation.RequestDigest) {
			return ErrConflict
		}
		var output any
		if invocation.Call.Name == "list_bots" {
			var query botmessaging.Query
			if decodeToolInput(invocation.Call.Arguments, &query, tool.schema) != nil || !validBotQuery(query) {
				result = botagent.ToolResult{IsError: true, Content: "INVALID_QUERY: Use valid bot IDs, cursor and tool names."}
			} else {
				output, err = listBotDirectory(ctx, tx, lease.UserID, lease.BotID, query, tool.maximum)
			}
		} else if invocation.Call.Name == "send_user_message" {
			var input botmessaging.UserMessage
			if decodeToolInput(invocation.Call.Arguments, &input, tool.schema) != nil || !validText(input.Message, 32<<10) {
				result = botagent.ToolResult{IsError: true, Content: "INVALID_MESSAGE: Provide nonempty text up to 32768 bytes. Nothing was sent."}
			} else {
				var automatic bool
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.bot_private_runs r JOIN tombot.bot_private_messages p ON p.user_id=r.user_id AND p.id=r.response_to WHERE r.user_id=$1 AND r.run_id=$2 AND p.deliver_to_user)`, lease.UserID, lease.RunID).Scan(&automatic)
				if err != nil {
					return err
				}
				if automatic {
					result = botagent.ToolResult{IsError: true, Content: "AUTOMATIC_DELIVERY: Your final answer will be sent directly to the user. Finish with the actual result; do not send a separate message."}
				} else {
					if err = lockDispatch(ctx, tx, lease.UserID); err != nil {
						return err
					}
					var message string
					message, err = writeDirectMessage(ctx, tx, lease.UserID, lease.BotID, []Block{{Kind: "text", Text: input.Message, UserMessage: true, SourceRunID: lease.RunID}})
					if errors.Is(err, ErrUnavailable) {
						result = botagent.ToolResult{IsError: true, Content: "USER_CHAT_UNAVAILABLE: Your user chat is unavailable. Nothing was sent."}
						err = nil
					} else if err == nil {
						output = map[string]string{"message_id": message, "status": "delivered"}
					}
				}
			}
		} else {
			var input botmessaging.Send
			if decodeToolInput(invocation.Call.Arguments, &input, tool.schema) != nil || !validID(input.BotID) || !validText(input.Message, botmessaging.MaxMessageBytes) || !validBotQuery(botmessaging.Query{BotID: input.BotID, RequiredTools: input.RequiredTools}) {
				result = botagent.ToolResult{IsError: true, Content: "INVALID_MESSAGE: Use a current bot ID, nonempty text up to 8192 bytes, and required tool names. Nothing was sent."}
			} else {
				var attempts int
				if err = tx.QueryRowContext(ctx, `UPDATE tombot.runs SET private_message_attempts=private_message_attempts+1 WHERE user_id=$1 AND id=$2 RETURNING private_message_attempts`, lease.UserID, lease.RunID).Scan(&attempts); err != nil {
					return err
				}
				if attempts > botmessaging.MaxCandidateAttempts {
					result = botagent.ToolResult{IsError: true, Content: "CANDIDATE_LIMIT: Three candidate attempts were already made. Do the work yourself with your available capabilities; do not wait or retry."}
				} else {
					if _, err = tx.ExecContext(ctx, `SAVEPOINT private_message_send`); err != nil {
						return err
					}
					output, err = tool.executor.sendPrivateMessage(ctx, tx, lease, operation.ID, input)
					var rejected privateRejection
					if errors.As(err, &rejected) || errors.Is(err, ErrCapacity) {
						if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT private_message_send`); rollbackErr != nil {
							return rollbackErr
						}
						code := string(rejected)
						if errors.Is(err, ErrCapacity) {
							code = "QUEUE_FULL"
						}
						switch code {
						case "MESSAGING_DISABLED", "CHAIN_LIMIT", "QUEUE_FULL", "MESSAGE_TOO_LARGE":
							// 整体限制与候选无关，换猫咪也不能解决，直接由发起方继续完成。
							output = map[string]any{"error": code, "sent": false, "next_action": "This limit applies to the request, not just the recipient. Do the work yourself with your available capabilities; do not retry with another cat."}
						default:
							alternatives, listErr := listBotDirectory(ctx, tx, lease.UserID, lease.BotID, botmessaging.Query{OnlyIdle: true, RequiredTools: input.RequiredTools}, tool.maximum-512)
							if listErr != nil && !errors.Is(listErr, ErrContextLimit) {
								return listErr
							}
							output = map[string]any{"error": code, "sent": false, "alternatives": alternatives, "next_action": "Choose another suitable idle cat with the same requirements. If none fits, do the work yourself. Never wait or repeatedly retry the busy cat."}
							if errors.Is(listErr, ErrContextLimit) {
								output = map[string]any{"error": code, "sent": false, "alternatives_lookup_required": true, "next_action": "Candidate profiles exceed this output budget. Use list_bots to narrow the query, or do the work yourself. Do not wait or retry the busy cat."}
							}
						}
						result.IsError = true
						err = nil
					}
					if err != nil {
						return err
					}
					if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT private_message_send`); err != nil {
						return err
					}
				}
			}
		}
		if errors.Is(err, ErrContextLimit) {
			result = botagent.ToolResult{IsError: true, Content: "DIRECTORY_TOO_LARGE: Query one bot by ID or use a larger tool output budget. No message was sent."}
			output = nil
			err = nil
		}
		if err != nil {
			return err
		}
		if output != nil {
			encoded, err = json.Marshal(output)
			if err != nil {
				return err
			}
			result.Content = string(encoded)
		}
		if err = result.Validate(tool.maximum); err != nil {
			return ErrContextLimit
		}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
		status, code := "succeeded", ""
		if result.IsError {
			status, code = "failed", "TOOL_FAILED"
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.run_operations SET status=$4,result=$5,error_code=$6,finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, operation.ID, status, string(encoded), code)
		return err
	})
	return result, err
}

type privateContext struct {
	chain, parent string
	path          []string
}

func readPrivateContext(ctx context.Context, tx *sql.Tx, lease Lease) (privateContext, error) {
	result := privateContext{chain: lease.RunID, path: []string{lease.BotID}}
	var path []byte
	err := tx.QueryRowContext(ctx, `SELECT chain_id,COALESCE(response_to,''),path FROM tombot.bot_private_runs WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&result.chain, &result.parent, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(path, &result.path)
}

// sendPrivateMessage 与所有聊天受理共享用户锁，检查空闲后立即入队，不保留等待对方空闲的请求。
func (service *Service) sendPrivateMessage(ctx context.Context, tx *sql.Tx, lease Lease, operation string, input botmessaging.Send) (any, error) {
	if err := lockDispatch(ctx, tx, lease.UserID); err != nil {
		return nil, err
	}
	var permitted bool
	if err := tx.QueryRowContext(ctx, `SELECT (messaging_policy->>'can_initiate')::boolean FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived FOR SHARE`, lease.UserID, lease.BotID).Scan(&permitted); err != nil {
		return nil, err
	}
	if !permitted {
		return nil, privateRejection("MESSAGING_DISABLED")
	}
	current, err := readPrivateContext(ctx, tx, lease)
	if err != nil {
		return nil, err
	}
	if slices.Contains(current.path, input.BotID) {
		return nil, privateRejection("SELF_OR_ANCESTOR")
	}
	if len(current.path) > botmessaging.MaxDepth {
		return nil, privateRejection("CHAIN_LIMIT")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1 AND chain_id=$2`, lease.UserID, current.chain).Scan(&count); err != nil {
		return nil, err
	}
	if count >= botmessaging.MaxMessages {
		return nil, privateRejection("CHAIN_LIMIT")
	}
	var target string
	err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.bots WHERE user_id=$1 AND id=$2 FOR SHARE`, lease.UserID, input.BotID).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, privateRejection("BOT_UNAVAILABLE")
	}
	if err != nil {
		return nil, err
	}
	page, err := listBotDirectory(ctx, tx, lease.UserID, lease.BotID, botmessaging.Query{BotID: target}, 32768)
	if err != nil {
		return nil, err
	}
	if len(page.Bots) != 1 {
		return nil, privateRejection("BOT_UNAVAILABLE")
	}
	bot := page.Bots[0]
	if !bot.AcceptDM || bot.Status == "archived" {
		return nil, privateRejection("BOT_UNAVAILABLE")
	}
	if !hasRequiredTools(bot, input.RequiredTools) {
		return nil, privateRejection("CAPABILITY_MISMATCH")
	}
	if bot.Status != "idle" {
		return nil, privateRejection("BOT_BUSY")
	}
	if len(input.Message)+1024 > service.options.ContextBytes {
		return nil, privateRejection("MESSAGE_TOO_LARGE")
	}
	if err = groupCapacity(ctx, tx, lease.UserID, 1); err != nil {
		return nil, err
	}
	first, second := lease.BotID, target
	if first > second {
		first, second = second, first
	}
	var conversation string
	err = tx.QueryRowContext(ctx, `INSERT INTO tombot.conversations(user_id,id,kind,bot_id,peer_bot_id,title) VALUES($1,$2,'bot_dm',$3,$4,'猫咪私信') ON CONFLICT(user_id,bot_id,peer_bot_id) WHERE kind='bot_dm' DO UPDATE SET title=tombot.conversations.title RETURNING id`, lease.UserID, ids.New(), first, second).Scan(&conversation)
	if err != nil {
		return nil, err
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT is_archived OR EXISTS(SELECT 1 FROM tombot.turns t WHERE t.user_id=c.user_id AND t.conversation_id=c.id AND t.status IN ('queued','running','canceling')) FROM tombot.conversations c WHERE user_id=$1 AND id=$2 FOR UPDATE`, lease.UserID, conversation).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, privateRejection("BOT_BUSY")
	}
	origin := lease.ConversationID
	var kind string
	if err = tx.QueryRowContext(ctx, `SELECT kind FROM tombot.conversations WHERE user_id=$1 AND id=$2`, lease.UserID, origin).Scan(&kind); err != nil {
		return nil, err
	}
	// 后台计划的结果回到该猫咪的主聊天，不为已完成的隐藏任务重新创建计划记录。
	if kind == "task" {
		if err = tx.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND bot_id=$2 AND kind='direct'`, lease.UserID, lease.BotID).Scan(&origin); err != nil {
			return nil, err
		}
	}
	var name string
	if err = tx.QueryRowContext(ctx, `SELECT name FROM tombot.bots WHERE user_id=$1 AND id=$2`, lease.UserID, lease.BotID).Scan(&name); err != nil {
		return nil, err
	}
	reference := botmessaging.Reference{MessageID: operation, SenderID: lease.BotID, SenderName: name, Kind: "request"}
	submitted, err := newPrivateInput(ctx, tx, lease.UserID, conversation, target, input.Message, reference, nil)
	if err != nil {
		return nil, err
	}
	path, err := json.Marshal(current.path)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.bot_private_messages(user_id,id,chain_id,parent_id,sender_bot_id,recipient_bot_id,source_run_id,source_conversation_id,source_path,conversation_id,request_message_id,deliver_to_user) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12)`, lease.UserID, operation, current.chain, current.parent, lease.BotID, target, lease.RunID, origin, path, conversation, submitted.MessageID, input.DeliverToUser); err != nil {
		return nil, err
	}
	current.parent = operation
	current.path = append(current.path, target)
	if err = attachPrivateContext(ctx, tx, lease.UserID, conversation, submitted, current); err != nil {
		return nil, err
	}
	if err = writePrivateNotices(ctx, tx, lease.UserID, lease.BotID, target, conversation); err != nil {
		return nil, err
	}
	reply := "Your peer's final response will resume you asynchronously. Do not poll or wait."
	if input.DeliverToUser {
		reply = "Your peer's final result will be delivered directly in its own chat with the user. End this turn or continue independent work; do not wait, poll or relay the result."
	}
	return map[string]string{"message_id": operation, "recipient_id": target, "status": "accepted", "reply": reply}, nil
}

// newPrivateInput 只由持有会话锁及用户受理锁的服务端调用，来源元数据和运行身份不接受模型覆盖。
func newPrivateInput(ctx context.Context, tx *sql.Tx, user, conversation, bot, text string, reference botmessaging.Reference, members []groupchat.Member) (Submission, error) {
	result := Submission{TurnID: ids.New(), MessageID: ids.New(), IsCreated: true}
	source := "bot_dm"
	if members != nil {
		source = "group"
	}
	var turnSequence, messageSequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1,message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING turn_sequence,message_sequence`, user, conversation).Scan(&turnSequence, &messageSequence); err != nil {
		return result, err
	}
	digest := sha256.Sum256([]byte(reference.MessageID + reference.Kind))
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,$4,$5,$6,$7,'queued')`, user, result.TurnID, conversation, turnSequence, ids.New(), digest[:], source); err != nil {
		return result, err
	}
	blocks, err := json.Marshal([]Block{{Kind: "text", Text: text, BotMessage: &reference}})
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status,visible_to_bot_id) VALUES($1,$2,$3,$4,$5,'system',$6,'complete',$7)`, user, result.MessageID, conversation, result.TurnID, messageSequence, blocks, bot); err != nil {
		return result, err
	}
	if err = event(ctx, tx, user, "private.message.received", result.MessageID, map[string]any{"bot_id": bot, "conversation_id": conversation, "sequence": messageSequence, "turn_id": result.TurnID}); err != nil {
		return result, err
	}
	if members != nil {
		runs, err := enqueueGroup(ctx, tx, user, conversation, result.TurnID, result.MessageID, 0, members, []string{bot})
		if err != nil {
			return result, err
		}
		result.RunID = runs[0]
		return result, nil
	}
	result.RunID = ids.New()
	inserted, err := tx.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status)
 SELECT user_id,$3,$4,$5,id,revision,jsonb_build_object('name',name,'description',description,'avatar_ref',avatar_ref,'category',category,'system_prompt',system_prompt,'tool_permissions',tool_permissions,'approval_mode',approval_mode,'messaging_policy',messaging_policy,'accent_hue',accent_hue),'queued' FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived`, user, bot, result.RunID, result.TurnID, conversation)
	if err != nil {
		return result, err
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return result, err
	}
	if count != 1 {
		return result, ErrUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, user, ids.New(), result.RunID, bot, conversation, result.TurnID); err != nil {
		return result, err
	}
	return result, event(ctx, tx, user, "run.queued", result.RunID, map[string]string{"conversation_id": conversation, "bot_id": bot, "turn_id": result.TurnID})
}

func attachPrivateContext(ctx context.Context, tx *sql.Tx, user, conversation string, submitted Submission, current privateContext) error {
	path, err := json.Marshal(current.path)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tombot.bot_private_runs(user_id,run_id,chain_id,response_to,path,trigger_message_id,conversation_id) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7)`, user, submitted.RunID, current.chain, current.parent, path, submitted.MessageID, conversation)
	return err
}
