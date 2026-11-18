package computeragent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type fileScope interface {
	TransferFile(context.Context, processgroup.Reference, computer.FileRequest, *os.File, *os.File) (computer.FileReceipt, error)
}

// TransferFile 使用桌面原授权和固定程序；文件内容不进入命令参数、模型回执或日志。
func (controller *Controller) TransferFile(ctx context.Context, request computer.FileRequest) (receipt computer.FileReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	failure = controller.withScope(ctx, request.Session, func(ctx context.Context, guard *desktopGuard) (err error) {
		scope, ok := guard.scope.(fileScope)
		if !ok {
			return errors.New("当前电脑代理未提供文件传输能力")
		}
		membership, err := guard.scope.Membership("shell")
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, membership.Close()) }()
		receipt, err = scope.TransferFile(ctx, *guard.state.Group, request, membership, guard.permit.reader)
		return err
	})
	return receipt, failure
}

type fileAttempt struct {
	initial computer.FileRequest
	receipt computer.FileReceipt
	content []byte
	failure error
}

// fileTransfers 每桌面最多登记 32 次传输、暂存 64 MiB；关闭或完成后清除暂存字节，保留消费屏障。
type fileTransfers struct {
	mu       sync.Mutex
	attempts map[string]*fileAttempt
	reserved int
	closed   bool
}

type fileExecutor func(context.Context, string, string, int, string, []byte) ([]byte, error)

func (transfers *fileTransfers) transfer(ctx context.Context, request computer.FileRequest, execute fileExecutor) (computer.FileReceipt, error) {
	transfers.mu.Lock()
	defer transfers.mu.Unlock()
	if request.Validate() != nil || execute == nil || transfers.closed {
		return computer.FileReceipt{}, errors.New("电脑文件传输已关闭或参数无效")
	}
	if err := ctx.Err(); err != nil {
		return computer.FileReceipt{}, err
	}
	if transfers.attempts == nil {
		transfers.attempts = make(map[string]*fileAttempt)
	}
	attempt := transfers.attempts[request.OperationID]
	start := request.Action == "import_start" || request.Action == "export_start"
	if start {
		if attempt != nil {
			return computer.FileReceipt{}, errors.New("电脑文件操作标识已经消费，不能重放启动")
		}
		if len(transfers.attempts) >= 32 || transfers.reserved+computer.MaximumFileBytes > 64<<20 {
			return computer.FileReceipt{}, errors.New("电脑文件暂存额度已用尽")
		}
		attempt = &fileAttempt{initial: request, receipt: computer.FileReceipt{Session: request.Session, OperationID: request.OperationID, Path: request.Path, Size: request.Size, SHA256: request.SHA256, State: "receiving"}}
		transfers.attempts[request.OperationID] = attempt
		transfers.reserved += computer.MaximumFileBytes
		if request.Action == "export_start" {
			relative, err := computer.FilePath(request.Path)
			if err != nil {
				attempt.failure = err
				return computer.FileReceipt{}, err
			}
			attempt.content, attempt.failure = execute(ctx, "read", relative, 0, "", nil)
			if attempt.failure != nil || len(attempt.content) > computer.MaximumFileBytes {
				clear(attempt.content)
				attempt.content = nil
				attempt.failure = errors.New("读取电脑文件未确认成功")
				return computer.FileReceipt{}, attempt.failure
			}
			digest := sha256.Sum256(attempt.content)
			attempt.receipt.Size, attempt.receipt.SHA256, attempt.receipt.State = len(attempt.content), hex.EncodeToString(digest[:]), "ready"
		}
		return attempt.receipt, nil
	}
	if attempt == nil || attempt.initial.Session != request.Session {
		return computer.FileReceipt{}, errors.New("电脑文件操作不存在或归属不符")
	}
	if request.Action == "close" {
		if attempt.receipt.State != "closed" {
			clear(attempt.content)
			attempt.content = nil
			transfers.reserved -= computer.MaximumFileBytes
			attempt.receipt.State = "closed"
		}
		return attempt.receipt, nil
	}
	if attempt.failure != nil {
		return computer.FileReceipt{}, attempt.failure
	}
	switch request.Action {
	case "import_chunk":
		if attempt.receipt.State != "receiving" || request.Offset != len(attempt.content) || len(request.Bytes) > attempt.receipt.Size-request.Offset {
			return computer.FileReceipt{}, errors.New("电脑文件分块重复、错序或越界")
		}
		attempt.content = append(attempt.content, request.Bytes...)
		attempt.receipt.Offset = len(attempt.content)
	case "import_commit":
		if attempt.receipt.State != "receiving" || len(attempt.content) != attempt.receipt.Size {
			return computer.FileReceipt{}, errors.New("电脑文件尚未完整接收或已经提交")
		}
		digest := sha256.Sum256(attempt.content)
		if hex.EncodeToString(digest[:]) != attempt.receipt.SHA256 {
			return computer.FileReceipt{}, errors.New("电脑文件内容摘要不符")
		}
		// 先消费提交权；传输中断后不得盲目重写目标文件。
		attempt.receipt.State = "committing"
		relative, err := computer.FilePath(attempt.receipt.Path)
		if err != nil {
			return computer.FileReceipt{}, err
		}
		_, attempt.failure = execute(ctx, "write", relative, attempt.receipt.Size, attempt.receipt.SHA256, attempt.content)
		clear(attempt.content)
		attempt.content = nil
		if attempt.failure != nil {
			return computer.FileReceipt{}, attempt.failure
		}
		attempt.receipt.State = "saved"
	case "export_chunk":
		if attempt.receipt.State != "ready" || request.Offset > len(attempt.content) {
			return computer.FileReceipt{}, errors.New("电脑文件快照不可读或分块越界")
		}
		receipt := attempt.receipt
		receipt.Offset = request.Offset
		receipt.Bytes = bytes.Clone(attempt.content[request.Offset:min(request.Offset+computer.MaximumFileChunkBytes, len(attempt.content))])
		return receipt, nil
	default:
		return computer.FileReceipt{}, errors.New("电脑文件动作无效")
	}
	return attempt.receipt, nil
}

func (transfers *fileTransfers) close() {
	transfers.mu.Lock()
	defer transfers.mu.Unlock()
	transfers.closed = true
	for _, attempt := range transfers.attempts {
		clear(attempt.content)
		attempt.content = nil
	}
	transfers.reserved = 0
}
