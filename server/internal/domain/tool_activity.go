package domain

// ToolInputPreview 限定聊天事件和快照可公开的参数摘要，历史记录中的其他字段不外发。
type ToolInputPreview struct {
	Tool          string `json:"tool,omitempty"`
	ArgumentBytes *int   `json:"argument_bytes,omitempty"`
	Path          string `json:"path,omitempty"`
}

// ToolActivity 是面向客户端的执行记录。参数摘要来自工具执行器的脱敏投影，step 从零开始。
type ToolActivity struct {
	ID           string           `json:"tool_call_id"`
	RunID        string           `json:"run_id"`
	Name         string           `json:"name"`
	Step         int              `json:"step"`
	Status       string           `json:"status"`
	DurationMS   *int64           `json:"duration_ms,omitempty"`
	ErrorCode    string           `json:"error_code,omitempty"`
	InputPreview ToolInputPreview `json:"input_preview"`
}
