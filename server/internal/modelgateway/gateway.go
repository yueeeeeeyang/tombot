package modelgateway

import (
	"context"
	"errors"
	"strings"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

// Input 只携带消息与工具，模型名称、地址和输出上限始终来自服务端固定策略。
type Input struct {
	System     string
	Messages   []provider.Message
	Tools      []provider.ToolSchema
	ToolChoice string
}

// Result 区分流是否完整与上游实际用量；nil 用量或计数字段明确表示未知。
type Result struct {
	IsComplete bool                    `json:"is_complete"`
	Usage      *provider.ReportedUsage `json:"usage"`
}

// Call 在独立受控连接中执行一次请求。回调必须遵守调用上下文，不能保留凭据或阻塞无限时间。
// 失败返回已接收的真实用量，外部执行器据此保留未知结果，不盲目重试或按零结算。
func (gateway *Gateway) Call(parent context.Context, configuration modelregistry.Configuration, credential []byte, input Input, emit func(provider.Event) error) (Result, error) {
	if configuration.Validate() != nil || emit == nil || len(credential) > 8192 || len(input.Messages) < 1 || len(input.Messages) > 1024 || len(input.Tools) > 128 || len(input.System) > 1<<20 || (!configuration.SupportsTools && len(input.Tools) > 0) {
		return Result{}, ErrInvalidInput
	}
	if input.ToolChoice != "" && input.ToolChoice != "auto" && input.ToolChoice != "required" {
		return Result{}, ErrInvalidInput
	}
	if input.ToolChoice != "" && len(input.Tools) == 0 {
		return Result{}, ErrInvalidInput
	}
	for _, value := range credential {
		if value < 33 || value > 126 {
			return Result{}, ErrInvalidInput
		}
	}
	contentBytes := int64(len(input.System))
	for _, message := range input.Messages {
		if len(message.Blocks) > 256 {
			return Result{}, ErrInvalidInput
		}
		for _, block := range message.Blocks {
			contentBytes += int64(len(block.Text))
			if block.Artifact != nil {
				contentBytes += int64(len(block.Artifact.Content))
			}
			if block.ToolResult != nil {
				contentBytes += int64(len(block.ToolResult.Content))
				if block.ToolResult.Artifact != nil {
					contentBytes += int64(len(block.ToolResult.Artifact.Content))
				}
			}
		}
	}
	for _, tool := range input.Tools {
		contentBytes += int64(len(tool.Name) + len(tool.Description) + len(tool.InputSchema))
	}
	if contentBytes > gateway.options.MaxRequestBytes {
		return Result{}, ErrRequestTooLarge
	}
	ctx, cancel := context.WithTimeout(parent, gateway.options.RequestTimeout)
	defer cancel()
	select {
	case gateway.slots <- struct{}{}:
		defer func() { <-gateway.slots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	path := "/chat/completions"
	if configuration.Protocol == "anthropic" {
		path = "/messages"
	}
	client, guard, err := gateway.client(strings.TrimRight(configuration.BaseURL, "/") + path)
	if err != nil {
		return Result{}, err
	}
	defer guard.transport.CloseIdleConnections()
	adapter := &provider.HTTP{Spec: domain.Provider{Kind: configuration.Protocol, BaseURL: configuration.BaseURL, IsEnabled: true}, Credential: credential, Client: client}
	request := provider.Request{Model: domain.Model{ModelKey: configuration.ModelKey, ContextWindow: configuration.ContextLimit, MaxOutput: configuration.OutputLimit, SupportsTools: configuration.SupportsTools, SupportsVision: configuration.SupportsVision}, System: input.System, Messages: input.Messages, Tools: input.Tools, ToolChoice: input.ToolChoice}
	var callbackErr error
	usage, err := adapter.Stream(ctx, request, func(event provider.Event) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		callbackErr = emit(event)
		return callbackErr
	})
	result := Result{IsComplete: err == nil, Usage: usage.Reported}
	if err == nil && guard.failure == nil {
		return result, nil
	}
	result.IsComplete = false
	if callbackErr != nil {
		return result, callbackErr
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if guard.failure != nil {
		return result, guard.failure
	}
	var failure *domain.Error
	if errors.As(err, &failure) && strings.HasPrefix(failure.Code, "PROVIDER_INVALID_") {
		return result, ErrProtocol
	}
	return result, ErrUnavailable
}
