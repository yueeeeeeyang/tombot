package provider

import (
	"context"
	"encoding/json"
	"io.yueyang/tom/internal/domain"
)

// Provider 隔离上游协议；回调错误必须停止读取并关闭上游请求。
type Provider interface {
	Stream(context.Context, Request, func(Event) error) (Usage, error)
}
type Request struct {
	Model      domain.Model
	System     string
	Messages   []Message
	Tools      []ToolSchema
	ToolChoice string
}
type Message struct {
	Role   string
	Blocks []domain.MessageBlock
}
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type Event struct {
	Kind       string
	Text       string
	ToolCallID string
	ToolName   string
	InputDelta string
	StopReason string
	Index      int
}
type Usage struct {
	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	CacheReadTokens int  `json:"cache_read_tokens"`
	Estimated       bool `json:"estimated"`
}
