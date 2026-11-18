package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
)

type chatFileTool struct {
	executor *Service
	schema   provider.ToolSchema
	maximum  int
}

func (tool chatFileTool) Schema() provider.ToolSchema { return tool.schema }
func (chatFileTool) UsesComputer() bool               { return false }
func (chatFileTool) RequiresVision() bool             { return false }

// Execute 将文件写入与工具回执原子提交，租约恢复时不会重复创建文件。
func (tool chatFileTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		if err := lockDispatch(ctx, tx, lease.UserID); err != nil {
			return err
		}
		conversation, _, err := libraryConversation(ctx, tx, lease)
		if err != nil {
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
		if invocation.Call.Name != tool.schema.Name || !chatlibrary.IsTool(invocation.Call.Name) || operation.Kind != "tool" || operation.Status != "started" || operation.Attempt != lease.Attempt || operation.FencingToken != lease.FencingToken || !bytes.Equal(operation.RequestDigest, digest[:]) {
			return ErrConflict
		}
		var input struct {
			Before  string `json:"before"`
			Query   string `json:"query"`
			ID      string `json:"id"`
			Offset  int    `json:"offset"`
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if decodeToolInput(invocation.Call.Arguments, &input, tool.schema) != nil {
			return chatlibrary.ErrInput
		}
		var output any
		switch invocation.Call.Name {
		case "chat_file_list":
			page, failure := chatlibrary.List(ctx, tx, lease.UserID, conversation, input.Before, input.Query)
			if failure != nil {
				return failure
			}
			if len(page.Files) > 5 {
				page.Files = page.Files[:5]
				page.Next = page.Files[4].ID
			}
			output = page
		case "chat_file_read":
			content, failure := chatlibrary.Download(ctx, tx, lease.UserID, conversation, input.ID)
			if failure != nil {
				return failure
			}
			if !utf8.Valid(content.Bytes) || strings.ContainsFunc(string(content.Bytes), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
				result = botagent.ToolResult{IsError: true, Content: "UNSUPPORTED_FILE: Only UTF-8 text can be read here. This binary or encoded file is available for user download. Do not invent its content."}
			} else {
				if input.Offset < 0 || input.Offset > len(content.Bytes) || input.Offset < len(content.Bytes) && !utf8.RuneStart(content.Bytes[input.Offset]) {
					return chatlibrary.ErrInput
				}
				end := min(len(content.Bytes), input.Offset+min(4096, max(4, tool.maximum/8)))
				for end > input.Offset && end < len(content.Bytes) && !utf8.RuneStart(content.Bytes[end]) {
					end--
				}
				var next *int
				if end < len(content.Bytes) {
					next = &end
				}
				output = map[string]any{"file": content.File, "text": string(content.Bytes[input.Offset:end]), "next_offset": next, "untrusted_content": true}
			}
		case "chat_file_create":
			if len(input.Content) > 64<<10 || !utf8.ValidString(input.Content) {
				return chatlibrary.ErrInput
			}
			file, failure := chatlibrary.Save(ctx, tx, lease.UserID, conversation, chatlibrary.Upload{ClientRequestID: operation.ID, Name: input.Name, Content: []byte(input.Content)})
			if failure != nil {
				return failure
			}
			if err = publishComputerFile(ctx, tx, lease, file); err != nil {
				return err
			}
			output = file
		}
		if err != nil {
			return err
		}
		if output != nil {
			encoded, err = json.Marshal(output)
			if err != nil {
				return err
			}
			result = botagent.ToolResult{Content: string(encoded)}
		}
		if err = result.Validate(tool.maximum); err != nil {
			return chatlibrary.ErrCapacity
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
	if err != nil {
		switch {
		case errors.Is(err, chatlibrary.ErrInput):
			return botagent.ToolResult{IsError: true, Content: "INVALID_FILE_INPUT: Use a valid file ID, UTF-8 byte offset, plain filename and bounded content."}, nil
		case errors.Is(err, chatlibrary.ErrNotFound):
			return botagent.ToolResult{IsError: true, Content: "FILE_NOT_FOUND: The conversation or file is unavailable to this run. List current files again."}, nil
		case errors.Is(err, chatlibrary.ErrConflict):
			return botagent.ToolResult{IsError: true, Content: "FILE_CONFLICT: The file has changed or was deleted. Nothing was saved."}, nil
		case errors.Is(err, chatlibrary.ErrCapacity):
			return botagent.ToolResult{IsError: true, Content: "FILE_CAPACITY: Storage or output limit exceeded. Nothing was saved."}, nil
		}
	}
	return result, err
}

// libraryConversation 群运行只能访问当前群；单聊、任务和私信只能投递到自己的用户单聊。
func libraryConversation(ctx context.Context, tx *sql.Tx, lease Lease) (string, bool, error) {
	isGroup, err := isGroupRun(ctx, tx, lease)
	if err != nil {
		return "", false, err
	}
	var conversation string
	if isGroup {
		err = tx.QueryRowContext(ctx, `SELECT c.id FROM tombot.conversations c JOIN tombot.group_deliveries d ON d.user_id=c.user_id AND d.conversation_id=c.id JOIN tombot.group_members m ON m.user_id=c.user_id AND m.group_id=c.group_id WHERE d.user_id=$1 AND d.run_id=$2 AND m.bot_id=$3 AND NOT c.is_archived`, lease.UserID, lease.RunID, lease.BotID).Scan(&conversation)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT c.id FROM tombot.conversations c JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id WHERE c.user_id=$1 AND c.bot_id=$2 AND c.kind='direct' AND NOT c.is_archived AND NOT b.is_archived`, lease.UserID, lease.BotID).Scan(&conversation)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = chatlibrary.ErrNotFound
	}
	return conversation, isGroup, err
}

// publishComputerFile 统一文本和二进制附件投递，群消息额度、引用与文件可见性一起提交。
func publishComputerFile(ctx context.Context, tx *sql.Tx, lease Lease, file chatlibrary.File) error {
	conversation, isGroup, err := libraryConversation(ctx, tx, lease)
	if err != nil {
		return err
	}
	if conversation != file.ConversationID {
		return ErrConflict
	}
	blocks := []Block{{Kind: "text", Text: file.Name, UserMessage: !isGroup, SourceRunID: lease.RunID}, {Kind: "chat_file", ChatFile: &file}}
	if isGroup {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.messages WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&count); err != nil {
			return err
		}
		if count >= groupchat.MaxPosts-1 {
			return chatlibrary.ErrCapacity
		}
		var trigger string
		if err = tx.QueryRowContext(ctx, `SELECT trigger_message_id FROM tombot.group_deliveries WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&trigger); err != nil {
			return err
		}
		reply, err := groupReplyReference(ctx, tx, lease, trigger)
		if err != nil {
			return err
		}
		if err = writeGroupMessage(ctx, tx, lease.UserID, conversation, lease.TurnID, lease.RunID, lease.BotID, ids.New(), reply, blocks, "complete"); err != nil {
			return err
		}
	} else if _, err = writeDirectMessage(ctx, tx, lease.UserID, lease.BotID, blocks); err != nil {
		return err
	}
	return chatlibrary.Publish(ctx, tx, lease.UserID, file)
}
