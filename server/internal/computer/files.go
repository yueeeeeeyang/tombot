package computer

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

const MaximumFileBytes = 16 << 20
const MaximumFileChunkBytes = 32 << 10
const DownloadDirectory = "/workspace/Downloads"

// FilePath 只接受远程业务目录内的规范路径；节点仍以 Bot 身份逐层拒绝符号链接。
func FilePath(value string) (string, error) {
	if !utf8.ValidString(value) || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n\\") {
		return "", errors.New("电脑文件路径无效")
	}
	if strings.HasPrefix(value, "/workspace/") {
		value = strings.TrimPrefix(value, "/workspace/")
	}
	if value == "" || value == "." || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") || strings.HasSuffix(strings.ToLower(value), ".crdownload") {
		return "", errors.New("电脑文件必须位于业务目录内")
	}
	return value, nil
}

// FileRequest 的操作标识来自持久账本；分块仅传输字节，不允许选择宿主目录或运行程序。
type FileRequest struct {
	Session     ExecutionSession `json:"session"`
	OperationID string           `json:"operation_id"`
	Action      string           `json:"action"`
	Path        string           `json:"path,omitempty"`
	Size        int              `json:"size,omitempty"`
	SHA256      string           `json:"sha256,omitempty"`
	Offset      int              `json:"offset,omitempty"`
	Bytes       []byte           `json:"bytes,omitempty"`
}

func (request FileRequest) Validate() error {
	if request.Session.Validate() != nil {
		return errors.New("电脑文件会话无效")
	}
	if _, err := ulid.ParseStrict(request.OperationID); err != nil {
		return errors.New("电脑文件操作标识无效")
	}
	valid := false
	switch request.Action {
	case "import_start":
		_, err := FilePath(request.Path)
		valid = err == nil && request.Size >= 0 && request.Size <= MaximumFileBytes && desktopDigestPattern.MatchString(request.SHA256) && request.Offset == 0 && len(request.Bytes) == 0
	case "export_start":
		_, err := FilePath(request.Path)
		valid = err == nil && request.Size == 0 && request.SHA256 == "" && request.Offset == 0 && len(request.Bytes) == 0
	case "import_chunk":
		valid = request.Path == "" && request.Size == 0 && request.SHA256 == "" && request.Offset >= 0 && request.Offset <= MaximumFileBytes && len(request.Bytes) > 0 && len(request.Bytes) <= MaximumFileChunkBytes && len(request.Bytes) <= MaximumFileBytes-request.Offset
	case "export_chunk":
		valid = request.Path == "" && request.Size == 0 && request.SHA256 == "" && request.Offset >= 0 && request.Offset <= MaximumFileBytes && len(request.Bytes) == 0
	case "import_commit", "close":
		valid = request.Path == "" && request.Size == 0 && request.SHA256 == "" && request.Offset == 0 && len(request.Bytes) == 0
	}
	if !valid {
		return errors.New("电脑文件动作或分块范围无效")
	}
	return nil
}

type FileReceipt struct {
	Session     ExecutionSession `json:"session"`
	OperationID string           `json:"operation_id"`
	State       string           `json:"state"`
	Path        string           `json:"path"`
	Size        int              `json:"size"`
	SHA256      string           `json:"sha256"`
	Offset      int              `json:"offset"`
	Bytes       []byte           `json:"bytes,omitempty"`
}

func (receipt FileReceipt) Validate(request FileRequest) error {
	if request.Validate() != nil || receipt.Session != request.Session || receipt.OperationID != request.OperationID || receipt.Size < 0 || receipt.Size > MaximumFileBytes || receipt.Offset < 0 || receipt.Offset > receipt.Size || !desktopDigestPattern.MatchString(receipt.SHA256) {
		return errors.New("电脑文件回执身份或范围无效")
	}
	if _, err := FilePath(receipt.Path); err != nil {
		return err
	}
	valid := false
	switch request.Action {
	case "import_start":
		valid = receipt.State == "receiving" && receipt.Path == request.Path && receipt.Size == request.Size && receipt.SHA256 == request.SHA256 && receipt.Offset == 0
	case "export_start":
		valid = receipt.State == "ready" && receipt.Path == request.Path && receipt.Offset == 0
	case "import_chunk":
		valid = receipt.State == "receiving" && receipt.Offset == request.Offset+len(request.Bytes)
	case "export_chunk":
		valid = receipt.State == "ready" && receipt.Offset == request.Offset && len(receipt.Bytes) == min(MaximumFileChunkBytes, receipt.Size-receipt.Offset)
	case "import_commit":
		valid = receipt.State == "saved" && receipt.Offset == receipt.Size
	case "close":
		valid = receipt.State == "closed"
	}
	if !valid || request.Action != "export_chunk" && len(receipt.Bytes) != 0 {
		return errors.New("电脑文件回执与动作不匹配")
	}
	return nil
}

type FileRuntime interface {
	TransferFile(context.Context, FileRequest) (FileReceipt, error)
}
