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
	InputTokens     int            `json:"input_tokens"`
	OutputTokens    int            `json:"output_tokens"`
	CacheReadTokens int            `json:"cache_read_tokens"`
	Estimated       bool           `json:"estimated"`
	Reported        *ReportedUsage `json:"reported,omitempty"`
}

// ReportedUsage 仅保留上游明确返回的计数；缺失字段为 nil，不用估值或零填充。
// OpenAI 兼容协议的输入包含缓存，Anthropic 输入计数不包含缓存，结算方必须按该标记解释。
type ReportedUsage struct {
	InputTokens        *int `json:"input_tokens"`
	OutputTokens       *int `json:"output_tokens"`
	CacheReadTokens    *int `json:"cache_read_tokens"`
	CacheWriteTokens   *int `json:"cache_write_tokens"`
	InputIncludesCache bool `json:"input_includes_cache"`
}
