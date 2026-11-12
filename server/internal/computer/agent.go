package computer

import (
	"context"
	"errors"
)

// AgentChunkBytes 保证字节块的 base64 编码仍能装入节点的有界控制帧。
const AgentChunkBytes = 32 << 10

type AgentWrite struct {
	Session ExecutionSession `json:"session"`
	Offset  int64            `json:"offset"`
	Bytes   []byte           `json:"bytes"`
}

type AgentRead struct {
	Session ExecutionSession `json:"session"`
	Offset  int64            `json:"offset"`
}

type AgentOutput struct {
	Offset   int64  `json:"offset"`
	Bytes    []byte `json:"bytes"`
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exit_code"`
}

func (request AgentWrite) Validate() error {
	if request.Session.Validate() != nil || request.Offset < 0 || request.Offset > 1<<40 || len(request.Bytes) == 0 || len(request.Bytes) > AgentChunkBytes {
		return errors.New("Bot Agent 输入块无效")
	}
	return nil
}

func (request AgentRead) Validate() error {
	if request.Session.Validate() != nil || request.Offset < 0 || request.Offset > 1<<40 {
		return errors.New("Bot Agent 输出游标无效")
	}
	return nil
}

func (output AgentOutput) Validate(offset int64) error {
	if len(output.Bytes) > AgentChunkBytes || output.Offset != offset+int64(len(output.Bytes)) || output.ExitCode < -1 || output.ExitCode > 255 || !output.Exited && output.ExitCode != -1 || output.Exited && output.ExitCode == -1 {
		return errors.New("Bot Agent 输出状态无效")
	}
	return nil
}

// AgentRuntime 仅运行固定 Bot Agent 程序；每次调用重新验证完整执行会话。
// 输入输出使用连续偏移；未知写入不重试，读取只能复查尚未推进的上一块。
type AgentRuntime interface {
	StartAgent(context.Context, ExecutionSession) error
	WriteAgent(context.Context, AgentWrite) (int64, error)
	ReadAgent(context.Context, AgentRead) (AgentOutput, error)
}
