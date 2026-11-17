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
	"io.yueyang/tom/internal/grouplibrary"
	"io.yueyang/tom/internal/provider"
)

type groupFileTool struct {
	executor *Service
	schema   provider.ToolSchema
	maximum  int
}

func (tool groupFileTool) Schema() provider.ToolSchema { return tool.schema }
func (groupFileTool) UsesComputer() bool               { return false }
func (groupFileTool) RequiresVision() bool             { return false }

// Execute 将文件写入与工具回执原子提交，租约恢复时不会重复创建文件。
func (tool groupFileTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	var result botagent.ToolResult
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		lease := invocation.Lease
		if err := available(ctx, tx, lease); err != nil {
			return err
		}
		if err := lockDispatch(ctx, tx, lease.UserID); err != nil {
			return err
		}
		var group string
		err := tx.QueryRowContext(ctx, `SELECT c.group_id FROM tombot.conversations c JOIN tombot.group_deliveries d ON d.user_id=c.user_id AND d.conversation_id=c.id JOIN tombot.group_members m ON m.user_id=c.user_id AND m.group_id=c.group_id WHERE d.user_id=$1 AND d.run_id=$2 AND m.bot_id=$3 AND NOT c.is_archived`, lease.UserID, lease.RunID, lease.BotID).Scan(&group)
		if errors.Is(err, sql.ErrNoRows) {
			return grouplibrary.ErrNotFound
		}
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
		if invocation.Call.Name != tool.schema.Name || !grouplibrary.IsTool(invocation.Call.Name) || operation.Kind != "tool" || operation.Status != "started" || operation.Attempt != lease.Attempt || operation.FencingToken != lease.FencingToken || !bytes.Equal(operation.RequestDigest, digest[:]) {
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
			return grouplibrary.ErrInput
		}
		var output any
		switch invocation.Call.Name {
		case "group_file_list":
			page, failure := grouplibrary.List(ctx, tx, lease.UserID, group, input.Before, input.Query)
			if failure != nil {
				return failure
			}
			if len(page.Files) > 5 {
				page.Files = page.Files[:5]
				page.Next = page.Files[4].ID
			}
			output = page
		case "group_file_read":
			content, failure := grouplibrary.Download(ctx, tx, lease.UserID, group, input.ID)
			if failure != nil {
				return failure
			}
			if !utf8.Valid(content.Bytes) || strings.ContainsFunc(string(content.Bytes), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
				result = botagent.ToolResult{IsError: true, Content: "UNSUPPORTED_FILE: Only UTF-8 text can be read here. This binary or encoded file is available for user download. Do not invent its content."}
			} else {
				if input.Offset < 0 || input.Offset > len(content.Bytes) || input.Offset < len(content.Bytes) && !utf8.RuneStart(content.Bytes[input.Offset]) {
					return grouplibrary.ErrInput
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
		case "group_file_create":
			if len(input.Content) > 64<<10 || !utf8.ValidString(input.Content) {
				return grouplibrary.ErrInput
			}
			output, err = grouplibrary.Save(ctx, tx, lease.UserID, group, grouplibrary.Upload{ClientRequestID: operation.ID, Name: input.Name, Content: []byte(input.Content)})
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
			return grouplibrary.ErrCapacity
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
		case errors.Is(err, grouplibrary.ErrInput):
			return botagent.ToolResult{IsError: true, Content: "INVALID_FILE_INPUT: Use a valid file ID, UTF-8 byte offset, plain filename and bounded content."}, nil
		case errors.Is(err, grouplibrary.ErrNotFound):
			return botagent.ToolResult{IsError: true, Content: "FILE_NOT_FOUND: The group or file is unavailable to this run. List current files again."}, nil
		case errors.Is(err, grouplibrary.ErrConflict):
			return botagent.ToolResult{IsError: true, Content: "FILE_CONFLICT: The file has changed or was deleted. Nothing was saved."}, nil
		case errors.Is(err, grouplibrary.ErrCapacity):
			return botagent.ToolResult{IsError: true, Content: "FILE_CAPACITY: Storage or output limit exceeded. Nothing was saved."}, nil
		}
	}
	return result, err
}
