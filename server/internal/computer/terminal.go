package computer

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

const (
	MaximumTerminalInputBytes = 16 << 10
	MaximumTerminalReadBytes  = 64 << 10
)

// TerminalAddress 使用完整执行身份和启动操作标识定位终端，不接受宿主 PID 或设备路径。
type TerminalAddress struct {
	Session     ExecutionSession `json:"session"`
	OperationID string           `json:"operation_id"`
}

func (address TerminalAddress) Validate() error {
	if address.Session.Validate() != nil {
		return errors.New("终端执行会话无效")
	}
	if _, err := ulid.ParseStrict(address.OperationID); err != nil {
		return errors.New("终端操作标识无效")
	}
	return nil
}

type TerminalStart struct {
	TerminalAddress
	Command ProcessCommand `json:"command"`
	Columns int            `json:"columns"`
	Rows    int            `json:"rows"`
}

func (request TerminalStart) Validate() error {
	return errors.Join(request.TerminalAddress.Validate(), request.Command.Validate(), ValidateTerminalSize(request.Columns, request.Rows))
}

func ValidateTerminalSize(columns, rows int) error {
	if columns < 2 || columns > 500 || rows < 2 || rows > 300 {
		return errors.New("终端尺寸须为 2 至 500 列、2 至 300 行")
	}
	return nil
}

type TerminalRead struct {
	TerminalAddress
	Cursor       int64 `json:"cursor"`
	MaximumBytes int   `json:"maximum_bytes"`
}

func (request TerminalRead) Validate() error {
	if request.Cursor < 0 || request.MaximumBytes < utf8.UTFMax || request.MaximumBytes > MaximumTerminalReadBytes {
		return errors.New("终端读取游标或字节上限无效")
	}
	return request.TerminalAddress.Validate()
}

// TerminalWrite 使用从 1 开始的连续序号，重复最近一次输入只返回原结果，不能再次写入。
type TerminalWrite struct {
	TerminalAddress
	Sequence int64  `json:"sequence"`
	Text     string `json:"text"`
}

func (request TerminalWrite) Validate() error {
	if request.Sequence < 1 || len(request.Text) < 1 || len(request.Text) > MaximumTerminalInputBytes || !utf8.ValidString(request.Text) {
		return errors.New("终端输入序号或内容无效")
	}
	return request.TerminalAddress.Validate()
}

type TerminalResize struct {
	TerminalAddress
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

func (request TerminalResize) Validate() error {
	return errors.Join(request.TerminalAddress.Validate(), ValidateTerminalSize(request.Columns, request.Rows))
}

// TerminalOutput 只报告当前输出与直接程序退出状态，不代表后台后代已从资源组回收。
type TerminalOutput struct {
	Text      string `json:"text"`
	Cursor    int64  `json:"cursor"`
	Truncated bool   `json:"truncated"`
	HasMore   bool   `json:"has_more"`
	Exited    bool   `json:"exited"`
	ExitCode  int    `json:"exit_code"`
	ErrorCode string `json:"error_code,omitempty"`
}

// Validate 校验游标、UTF-8 和退出状态，供节点协议与模型工具共用，拒绝伪造完整输出。
func (output TerminalOutput) Validate(cursor int64, maximum int) error {
	if cursor < 0 || maximum < utf8.UTFMax || maximum > MaximumTerminalReadBytes || output.Cursor < cursor || len(output.Text) > maximum || !utf8.ValidString(output.Text) || output.Cursor-cursor < int64(len(output.Text)) || !output.Truncated && output.Cursor-cursor != int64(len(output.Text)) || output.HasMore && output.Text == "" || output.ExitCode < -1 || output.ExitCode > 255 || !output.Exited && output.ExitCode != -1 || output.ErrorCode != "" && output.ErrorCode != "terminal_read_failed" && output.ErrorCode != "terminal_wait_failed" {
		return errors.New("电脑终端输出无效")
	}
	return nil
}

type TerminalReceipt struct {
	Sequence     int64  `json:"sequence"`
	WrittenBytes int    `json:"written_bytes"`
	ErrorCode    string `json:"error_code,omitempty"`
}

// Validate 保留部分输入的实际回执；缺少错误标志的短写不能作为已完整执行返回。
func (receipt TerminalReceipt) Validate(request TerminalWrite) error {
	if receipt.Sequence != request.Sequence || receipt.WrittenBytes < 0 || receipt.WrittenBytes > len(request.Text) || receipt.ErrorCode != "" && receipt.ErrorCode != "terminal_write_incomplete" || receipt.ErrorCode == "" && receipt.WrittenBytes != len(request.Text) {
		return errors.New("电脑终端输入回执无效")
	}
	return nil
}

// TerminalRuntime 只能用于已经取得执行会话的受信工作者；输入输出不得绕过用户与桌面归属检查。
type TerminalRuntime interface {
	StartTerminal(context.Context, TerminalStart) (TerminalOutput, error)
	ReadTerminal(context.Context, TerminalRead) (TerminalOutput, error)
	WriteTerminal(context.Context, TerminalWrite) (TerminalReceipt, error)
	ResizeTerminal(context.Context, TerminalResize) error
}
