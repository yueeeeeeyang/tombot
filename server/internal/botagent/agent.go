// Package botagent 实现每个 Bot 独立进程中的模型与工具循环，不持有平台凭据或数据库连接。
package botagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

var (
	ErrInvalidInput   = errors.New("Bot Agent 启动内容无效")
	ErrModelOutput    = errors.New("模型返回了无效或超过限制的执行内容")
	ErrContextLimit   = errors.New("Bot Agent 上下文超过限制")
	ErrOperationLimit = errors.New("Bot Agent 模型轮次超过限制")
	ErrProtocol       = errors.New("Bot Agent 通信未确认，不能重放操作")
)

// ToolCall 保留参数原文，避免持久恢复或跨进程编码改变大整数及请求摘要。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ModelOutput struct {
	Text       string     `json:"text"`
	Tools      []ToolCall `json:"tools"`
	StopReason string     `json:"stop_reason"`
}

type ToolResult struct {
	Content  string              `json:"content"`
	IsError  bool                `json:"is_error"`
	Artifact *domain.ArtifactRef `json:"artifact,omitempty"`
}

// Limits 由服务端确定；模型和工具输出在进入下一轮上下文前再次校验。
type Limits struct {
	ModelSteps   int `json:"model_steps"`
	ToolCalls    int `json:"tool_calls"`
	OutputBytes  int `json:"output_bytes"`
	ContextBytes int `json:"context_bytes"`
}

type Start struct {
	Input      modelgateway.Input `json:"input"`
	Limits     Limits             `json:"limits"`
	LocalTools []string           `json:"local_tools,omitempty"`
}

// Relay 必须先确认操作已持久登记，再执行外部动作。出错后 Agent 不自动重试。
// 模型选择、授权、记账和未知结果恢复由服务端负责，不随启动内容下发。
type Relay interface {
	Model(context.Context, string, modelgateway.Input) (ModelOutput, error)
	Tool(context.Context, string, ToolCall) (ToolResult, error)
}

func textValid(text string, maximum int) bool {
	return len(text) <= maximum && utf8.ValidString(text) && !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })
}

// Validate 检查固定边界和初始上下文，不接收供应商地址、模型名称或凭据。
func (start Start) Validate() error {
	limits := start.Limits
	if limits.ModelSteps < 1 || limits.ModelSteps > 64 || limits.ToolCalls < 1 || limits.ToolCalls > 16 || limits.OutputBytes < 1024 || limits.OutputBytes > 24<<10 || limits.ContextBytes < 1024 || limits.ContextBytes > 1<<20 || len(start.Input.Messages) == 0 || len(start.Input.Messages) > 200 || len(start.Input.Tools) > 128 || start.Input.ToolChoice != "" {
		return ErrInvalidInput
	}
	if !textValid(start.Input.System, limits.ContextBytes) {
		return ErrInvalidInput
	}
	seen := map[string]bool{}
	for _, tool := range start.Input.Tools {
		var schema map[string]json.RawMessage
		if !bots.ValidToolName(tool.Name) || seen[tool.Name] || !textValid(tool.Description, 4096) || strings.TrimSpace(tool.Description) == "" || len(tool.InputSchema) > 16<<10 || json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
			return ErrInvalidInput
		}
		seen[tool.Name] = true
	}
	local := map[string]bool{}
	for _, name := range start.LocalTools {
		if !seen[name] || local[name] {
			return ErrInvalidInput
		}
		local[name] = true
	}
	for _, message := range start.Input.Messages {
		if message.Role != "user" && message.Role != "assistant" && message.Role != "system" || len(message.Blocks) == 0 || len(message.Blocks) > 256 {
			return ErrInvalidInput
		}
		for _, block := range message.Blocks {
			if block.Kind != "text" || !textValid(block.Text, limits.ContextBytes) || block.ToolUse != nil || block.ToolResult != nil || block.Artifact != nil || block.Plan != nil || block.Handoff != nil {
				return ErrInvalidInput
			}
		}
	}
	return checkContext(start.Input, limits.ContextBytes)
}

func checkContext(input modelgateway.Input, maximum int) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return ErrInvalidInput
	}
	if len(encoded) > maximum {
		return ErrContextLimit
	}
	return nil
}

func validateOutput(output ModelOutput, start Start, allowed map[string]bool) error {
	if !textValid(output.Text, start.Limits.OutputBytes) || len(output.Tools) > start.Limits.ToolCalls || strings.TrimSpace(output.Text) == "" && len(output.Tools) == 0 {
		return ErrModelOutput
	}
	switch output.StopReason {
	case "stop", "end_turn", "tool_use", "tool_calls", "stop_sequence":
	default:
		return ErrModelOutput
	}
	seen := map[string]bool{}
	for _, call := range output.Tools {
		var arguments map[string]json.RawMessage
		if !allowed[call.Name] || seen[call.ID] || !textValid(call.ID, 256) || strings.TrimSpace(call.ID) == "" || len(call.Arguments) > start.Limits.OutputBytes || json.Unmarshal([]byte(call.Arguments), &arguments) != nil || arguments == nil {
			return ErrModelOutput
		}
		seen[call.ID] = true
	}
	encoded, err := json.Marshal(output)
	if err != nil || len(encoded) > 56<<10 {
		return ErrModelOutput
	}
	return nil
}

// Run 执行单个运行的有界推理循环。任一步失败即停止；持久恢复必须经过受限 Relay。
// 调用方应在所属容器的独立进程内调用，不能用本函数作为电脑失联后的服务端回退。
func Run(ctx context.Context, start Start, relay Relay) (string, error) {
	if relay == nil {
		return "", ErrInvalidInput
	}
	if err := start.Validate(); err != nil {
		return "", err
	}
	input := start.Input
	// 追加消息使用独立切片，不能改写调用者保存的启动快照。
	input.Messages = append(input.Messages[:0:0], input.Messages...)
	allowed := map[string]bool{}
	for _, tool := range input.Tools {
		allowed[tool.Name] = true
	}
	for step := 1; step <= start.Limits.ModelSteps; step++ {
		if err := context.Cause(ctx); err != nil {
			return "", err
		}
		if err := checkContext(input, start.Limits.ContextBytes); err != nil {
			return "", err
		}
		output, err := relay.Model(ctx, fmt.Sprintf("model.%d", step), input)
		if err != nil {
			return output.Text, err
		}
		if err = context.Cause(ctx); err != nil {
			return output.Text, err
		}
		if err = validateOutput(output, start, allowed); err != nil {
			return output.Text, err
		}
		if len(output.Tools) == 0 {
			return output.Text, nil
		}
		input.Messages = append(input.Messages, assistantMessage(output))
		results := make([]domain.MessageBlock, 0, len(output.Tools))
		for index, call := range output.Tools {
			if err = context.Cause(ctx); err != nil {
				return output.Text, err
			}
			result, err := relay.Tool(ctx, fmt.Sprintf("tool.%d.%d", step, index+1), call)
			if err != nil {
				return output.Text, err
			}
			if result.Validate(start.Limits.OutputBytes) != nil {
				return output.Text, ErrProtocol
			}
			results = append(results, toolBlock(call, result))
		}
		input.Messages = append(input.Messages, provider.Message{Role: "user", Blocks: results})
	}
	return "", ErrOperationLimit
}

// assistantMessage 与 toolBlock 统一生成执行和中继审计使用的上下文，保持恢复摘要一致。
func assistantMessage(output ModelOutput) provider.Message {
	blocks := []domain.MessageBlock{}
	if output.Text != "" {
		blocks = append(blocks, domain.MessageBlock{Kind: "text", Text: output.Text})
	}
	for _, call := range output.Tools {
		blocks = append(blocks, domain.MessageBlock{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: call.ID, Name: call.Name, Input: json.RawMessage(call.Arguments)}})
	}
	return provider.Message{Role: "assistant", Blocks: blocks}
}
func toolBlock(call ToolCall, result ToolResult) domain.MessageBlock {
	return domain.MessageBlock{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: call.ID, Content: result.Content, IsError: result.IsError, Artifact: result.Artifact}}
}
