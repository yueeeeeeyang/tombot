package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botmemory"
	"io.yueyang/tom/internal/provider"
)

type memoryTool struct {
	executor *Service
	schema   provider.ToolSchema
	maximum  int
}

func (tool memoryTool) Schema() provider.ToolSchema { return tool.schema }
func (memoryTool) UsesComputer() bool               { return false }
func (memoryTool) RequiresVision() bool             { return false }

func decodeMemoryInput(arguments string, target any, schema provider.ToolSchema) error {
	var properties map[string]json.RawMessage
	var contract struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal([]byte(arguments), &properties) != nil || properties == nil {
		return botmemory.ErrInput
	}
	if err := json.Unmarshal(schema.InputSchema, &contract); err != nil {
		return err
	}
	for name := range properties {
		if _, ok := contract.Properties[name]; !ok {
			return botmemory.ErrInput
		}
	}
	for _, name := range contract.Required {
		if value, ok := properties[name]; !ok || string(value) == "null" {
			return botmemory.ErrInput
		}
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return botmemory.ErrInput
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return botmemory.ErrInput
	}
	return nil
}

// memoryPermitted 只在启用工具的私人单聊开放记忆；群聊、机器人私信不能读取用户画像。
func memoryPermitted(ctx context.Context, tx *sql.Tx, lease Lease) (bool, error) {
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.conversations c JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id
		WHERE c.user_id=$1 AND c.id=$2 AND c.bot_id=$3 AND c.kind='direct' AND NOT c.is_archived AND NOT b.is_archived AND jsonb_array_length(b.tool_permissions)>0)`, lease.UserID, lease.ConversationID, lease.BotID).Scan(&allowed)
	return allowed, err
}

// memorySource 仅接受已经进入模型输入水位、属于当前回合的用户正文，引用及表情不是证据。
func memorySource(ctx context.Context, tx *sql.Tx, lease Lease, change botmemory.Change) (botmemory.Source, error) {
	if !validID(change.SourceMessageID) {
		return botmemory.Source{}, botmemory.ErrSource
	}
	var source botmemory.Source
	var encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT m.id,m.sequence,m.blocks FROM tombot.messages m JOIN tombot.runs r ON r.user_id=m.user_id AND r.id=$5
		WHERE m.user_id=$1 AND m.conversation_id=$2 AND m.turn_id=$3 AND m.id=$4 AND m.sender_kind='user' AND m.status='complete' AND m.sequence<=r.input_sequence`,
		lease.UserID, lease.ConversationID, lease.TurnID, change.SourceMessageID, lease.RunID).Scan(&source.MessageID, &source.Sequence, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return source, botmemory.ErrSource
	}
	if err != nil {
		return source, err
	}
	var blocks []Block
	if err = json.Unmarshal(encoded, &blocks); err != nil {
		return source, err
	}
	for _, block := range blocks {
		if block.Kind == "text" && strings.Contains(block.Text, change.Evidence) {
			return source, nil
		}
	}
	return source, botmemory.ErrSource
}

// Execute 把记忆变更与成功工具回执放在同一事务中，防止重试重复写入或报出未持久化的成功。
func (tool memoryTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	if invocation.Call.Name != tool.schema.Name || !botmemory.IsTool(invocation.Call.Name) {
		return botagent.ToolResult{}, ErrInvalidInput
	}
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		allowed, err := memoryPermitted(ctx, tx, lease)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrUnavailable
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
		if operation.Kind != "tool" || !bytes.Equal(operation.RequestDigest, digest[:]) || operation.Status != "started" || operation.Attempt != lease.Attempt || operation.FencingToken != lease.FencingToken {
			return ErrConflict
		}
		var output any
		if invocation.Call.Name == "memory_search" {
			var query botmemory.Query
			if err = decodeMemoryInput(invocation.Call.Arguments, &query, tool.schema); err != nil {
				return err
			}
			output, err = botmemory.Search(ctx, tx, lease.UserID, lease.BotID, query)
		} else {
			var change botmemory.Change
			if err = decodeMemoryInput(invocation.Call.Arguments, &change, tool.schema); err != nil {
				return err
			}
			deleting := invocation.Call.Name == "memory_delete"
			if err = change.Validate(deleting); err != nil {
				return err
			}
			source, sourceErr := memorySource(ctx, tx, lease, change)
			if sourceErr != nil {
				return sourceErr
			}
			output, err = botmemory.Apply(ctx, tx, lease.UserID, lease.BotID, change, source, deleting)
		}
		if err != nil {
			return err
		}
		encoded, err = json.Marshal(output)
		if err != nil {
			return err
		}
		result = botagent.ToolResult{Content: string(encoded)}
		if err = result.Validate(tool.maximum); err != nil {
			return botmemory.ErrCapacity
		}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.run_operations SET status='succeeded',result=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND id=$3`, lease.UserID, lease.RunID, operation.ID, string(encoded))
		return err
	})
	if err != nil {
		message := ""
		switch {
		case errors.Is(err, botmemory.ErrInput):
			message = "INVALID_MEMORY: Follow the schema and byte limits; use plain concise text."
		case errors.Is(err, botmemory.ErrSource):
			message = "INVALID_SOURCE: Cite verbatim user text from this direct-chat turn already delivered to you. Forgotten or superseded sources cannot be reused."
		case errors.Is(err, botmemory.ErrConflict):
			message = "MEMORY_CONFLICT: Search again and use the current revision. No memory changed."
		case errors.Is(err, botmemory.ErrCapacity):
			message = "MEMORY_CAPACITY: Storage or result budget exceeded. Narrow the search, consolidate existing topics, or ask what to forget. No memory changed."
		}
		if message != "" {
			return botagent.ToolResult{IsError: true, Content: message}, nil
		}
		return botagent.ToolResult{}, err
	}
	return result, nil
}
