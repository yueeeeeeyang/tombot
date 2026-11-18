package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/chatlibrary"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/provider"
)

type chatComputerFileTool struct {
	executor *Service
	runtime  computer.FileRuntime
	schema   provider.ToolSchema
	timeout  time.Duration
}

func (tool chatComputerFileTool) Schema() provider.ToolSchema { return tool.schema }
func (chatComputerFileTool) UsesComputer() bool               { return true }
func (chatComputerFileTool) RequiresVision() bool             { return false }

// Execute 分开数据库预检、受限节点传输和最终提交；不跨网络持有数据库事务。
func (tool chatComputerFileTool) Execute(ctx context.Context, invocation Invocation) (result botagent.ToolResult, failure error) {
	var input struct {
		ID   string `json:"id"`
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if invocation.Call.Name != tool.schema.Name || invocation.Computer == nil || invocation.Computer.Validate() != nil || invocation.Computer.UserID != invocation.Lease.UserID || invocation.Computer.RunID != invocation.Lease.RunID || invocation.Computer.BotID != invocation.Lease.BotID || decodeToolInput(invocation.Call.Arguments, &input, tool.schema) != nil {
		return botagent.ToolResult{IsError: true, Content: "INVALID_FILE_INPUT: Use a current chat file ID or a regular file path inside /workspace."}, nil
	}
	var source chatlibrary.Content
	var conversation string
	err := tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		if err := checkFileOperation(ctx, tx, invocation); err != nil {
			return err
		}
		var err error
		conversation, _, err = libraryConversation(ctx, tx, invocation.Lease)
		if err != nil {
			return err
		}
		if tool.schema.Name == "chat_file_import" {
			source, err = chatlibrary.Download(ctx, tx, invocation.Lease.UserID, conversation, input.ID)
			return err
		}
		return nil
	})
	if err != nil {
		return botagent.ToolResult{}, err
	}
	defer clear(source.Bytes)
	request := computer.FileRequest{Session: *invocation.Computer, OperationID: invocation.OperationID}
	if tool.schema.Name == "chat_file_import" {
		request.Action = "import_start"
		request.Path = path.Join("/workspace/Library", invocation.OperationID, source.File.Name)
		request.Size, request.SHA256 = len(source.Bytes), source.File.SHA256
	} else {
		request.Action, request.Path = "export_start", input.Path
	}
	if err := request.Validate(); err != nil {
		return botagent.ToolResult{IsError: true, Content: "INVALID_FILE_PATH: The file must be inside /workspace."}, nil
	}
	call := func(request computer.FileRequest) (computer.FileReceipt, error) {
		bounded, cancel := context.WithTimeout(ctx, tool.timeout)
		defer cancel()
		receipt, err := tool.runtime.TransferFile(bounded, request)
		if err != nil {
			return receipt, err
		}
		if err = receipt.Validate(request); err != nil {
			return computer.FileReceipt{}, err
		}
		return receipt, nil
	}
	first, err := call(request)
	if err != nil {
		return botagent.ToolResult{}, err
	}
	defer func() {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), tool.timeout)
		defer cancel()
		closeRequest := computer.FileRequest{Session: request.Session, OperationID: request.OperationID, Action: "close"}
		receipt, err := tool.runtime.TransferFile(bounded, closeRequest)
		if err == nil {
			err = receipt.Validate(closeRequest)
		}
		failure = errors.Join(failure, err)
	}()
	transfer := func(action string, offset int, content []byte) (computer.FileReceipt, error) {
		next := computer.FileRequest{Session: request.Session, OperationID: request.OperationID, Action: action, Offset: offset, Bytes: content}
		receipt, err := call(next)
		if err == nil && (receipt.Path != first.Path || receipt.Size != first.Size || receipt.SHA256 != first.SHA256) {
			err = errors.New("电脑文件传输快照发生变化")
		}
		return receipt, err
	}
	if request.Action == "import_start" {
		for offset := 0; offset < len(source.Bytes); offset += computer.MaximumFileChunkBytes {
			if _, err = transfer("import_chunk", offset, source.Bytes[offset:min(offset+computer.MaximumFileChunkBytes, len(source.Bytes))]); err != nil {
				return botagent.ToolResult{}, err
			}
		}
		if _, err = transfer("import_commit", 0, nil); err != nil {
			return botagent.ToolResult{}, err
		}
		encoded, err := json.Marshal(map[string]any{"file_id": source.File.ID, "path": first.Path, "size": first.Size, "sha256": first.SHA256})
		return botagent.ToolResult{Content: string(encoded)}, err
	}
	content := make([]byte, 0, first.Size)
	defer func() { clear(content) }()
	for offset := 0; offset < first.Size; offset += computer.MaximumFileChunkBytes {
		receipt, err := transfer("export_chunk", offset, nil)
		if err != nil {
			return botagent.ToolResult{}, err
		}
		content = append(content, receipt.Bytes...)
	}
	digest := sha256.Sum256(content)
	if len(content) != first.Size || hex.EncodeToString(digest[:]) != first.SHA256 {
		return botagent.ToolResult{}, errors.New("电脑文件完整性校验失败")
	}
	err = tool.executor.worker(ctx, invocation.Lease, false, func(tx *sql.Tx) error {
		if err := checkFileOperation(ctx, tx, invocation); err != nil {
			return err
		}
		file, err := chatlibrary.Save(ctx, tx, invocation.Lease.UserID, conversation, chatlibrary.Upload{ClientRequestID: invocation.OperationID, Name: input.Name, Content: content})
		if err != nil {
			return err
		}
		if err = publishComputerFile(ctx, tx, invocation.Lease, file); err != nil {
			return err
		}
		encoded, err := json.Marshal(file)
		if err != nil {
			return err
		}
		result = botagent.ToolResult{Content: string(encoded)}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombot.run_operations SET status='succeeded',result=$4,finished_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2 AND id=$3`, invocation.Lease.UserID, invocation.Lease.RunID, invocation.OperationID, string(encoded))
		return err
	})
	return result, err
}

func checkFileOperation(ctx context.Context, tx *sql.Tx, invocation Invocation) error {
	if err := available(ctx, tx, invocation.Lease); err != nil {
		return err
	}
	if err := lockDispatch(ctx, tx, invocation.Lease.UserID); err != nil {
		return err
	}
	operation, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND id=$3 AND computer_session_id=$4 FOR UPDATE`, invocation.Lease.UserID, invocation.Lease.RunID, invocation.OperationID, invocation.Computer.SessionID))
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(invocation.Call)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	if operation.Kind != "computer" || operation.Status != "started" || operation.FencingToken != invocation.Lease.FencingToken || operation.Attempt != invocation.Lease.Attempt || !bytes.Equal(operation.RequestDigest, digest[:]) {
		return ErrConflict
	}
	return nil
}
