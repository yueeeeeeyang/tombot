package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

// appendInput 在持有会话锁时追加消息；每回合有界，受理后即使客户端退出也不会丢失。
func (service *Service) appendInput(ctx context.Context, tx *sql.Tx, userID, conversationID, turnID, runID string, request SendRequest, digest []byte, submitted *Submission) error {
	blocks, err := json.Marshal([]Block{{Kind: "text", Text: request.Content}})
	if err != nil {
		return err
	}
	var count, used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(octet_length(m.blocks::text)),0) FROM tombot.run_inputs i JOIN tombot.messages m ON m.user_id=i.user_id AND m.id=i.message_id WHERE i.user_id=$1 AND i.turn_id=$2`, userID, turnID).Scan(&count, &used); err != nil {
		return err
	}
	// 给上一轮回复保留上下文空间，避免续跑时截断已经受理的补充消息。
	if count >= service.options.ContextMessages-1 || used+len(blocks) > service.options.ContextBytes/2 {
		return ErrCapacity
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET message_sequence=message_sequence+1,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2 RETURNING message_sequence`, userID, conversationID).Scan(&sequence); err != nil {
		return err
	}
	*submitted = Submission{TurnID: turnID, RunID: runID, MessageID: ids.New(), IsCreated: true}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,$5,'user',$6,'complete')`, userID, submitted.MessageID, conversationID, turnID, sequence, blocks); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.run_inputs(user_id,client_request_id,request_digest,run_id,turn_id,conversation_id,message_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, userID, request.ClientRequestID, digest, runID, turnID, conversationID, submitted.MessageID); err != nil {
		return err
	}
	return event(ctx, tx, userID, "message.created", submitted.MessageID, map[string]any{"conversation_id": conversationID, "sequence": sequence, "turn_id": turnID})
}

// mergeRunInputs 在模型调用边界固定新增消息及其插入位置，不拆开工具调用与结果。
// Agent 保持原始执行上下文；服务端每次按持久批次合并，恢复也得到相同的请求摘要。
func (service *Service) mergeRunInputs(ctx context.Context, lease Lease, key string, input modelgateway.Input) (modelgateway.Input, error) {
	step, err := strconv.Atoi(strings.TrimPrefix(key, "model."))
	if err != nil || key != "model."+strconv.Itoa(step) || step < 1 || step > 64 {
		return input, ErrInvalidInput
	}
	merged := input
	err = service.worker(ctx, lease, false, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.run_input_batches WHERE user_id=$1 AND run_id=$2 AND step_key=$3)`, lease.UserID, lease.RunID, key).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			var cursor int64
			if err := tx.QueryRowContext(ctx, `SELECT input_sequence FROM tombot.runs WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID).Scan(&cursor); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT m.sequence,m.blocks FROM tombot.messages m JOIN tombot.run_inputs i ON i.user_id=m.user_id AND i.message_id=m.id WHERE m.user_id=$1 AND m.turn_id=$2 AND m.sequence>$3 ORDER BY m.sequence LIMIT $4`, lease.UserID, lease.TurnID, cursor, service.options.ContextMessages)
			if err != nil {
				return err
			}
			messages := []provider.Message{}
			for rows.Next() {
				var blocks []byte
				if err = rows.Scan(&cursor, &blocks); err != nil {
					return errors.Join(err, rows.Close())
				}
				var parsed []domain.MessageBlock
				if err = json.Unmarshal(blocks, &parsed); err != nil {
					return errors.Join(err, rows.Close())
				}
				messages = append(messages, provider.Message{Role: "user", Blocks: parsed})
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			encoded, err := json.Marshal(messages)
			if err != nil {
				return err
			}
			if len(encoded) > service.options.ContextBytes {
				return ErrContextLimit
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.run_input_batches(user_id,run_id,step_key,step,message_offset,messages) VALUES($1,$2,$3,$4,$5,$6)`, lease.UserID, lease.RunID, key, step, len(input.Messages), encoded); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tombot.runs SET input_sequence=$3 WHERE user_id=$1 AND id=$2`, lease.UserID, lease.RunID, cursor); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT message_offset,messages FROM tombot.run_input_batches WHERE user_id=$1 AND run_id=$2 AND step<=$3 ORDER BY step LIMIT 64`, lease.UserID, lease.RunID, step)
		if err != nil {
			return err
		}
		merged.Messages = make([]provider.Message, 0, len(input.Messages))
		position := 0
		for rows.Next() {
			var offset int
			var encoded []byte
			if err = rows.Scan(&offset, &encoded); err != nil {
				return errors.Join(err, rows.Close())
			}
			if offset < position || offset > len(input.Messages) {
				return errors.Join(ErrConflict, rows.Close())
			}
			var messages []provider.Message
			if err = json.Unmarshal(encoded, &messages); err != nil {
				return errors.Join(err, rows.Close())
			}
			merged.Messages = append(merged.Messages, input.Messages[position:offset]...)
			merged.Messages = append(merged.Messages, messages...)
			position = offset
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		merged.Messages = append(merged.Messages, input.Messages[position:]...)
		encoded, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if len(encoded) > service.options.ContextBytes {
			return ErrContextLimit
		}
		return nil
	})
	return merged, err
}
