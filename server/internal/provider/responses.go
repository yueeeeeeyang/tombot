package provider

import (
	"encoding/json"
	"io.yueyang/tom/internal/domain"
)

// encodeResponses 使用无状态完整历史；工具调用与结果按 call_id 配对，禁止让上游持久化会话。
func encodeResponses(request Request) ([]byte, error) {
	items := []any{}
	for _, message := range request.Messages {
		if message.Role != "user" && message.Role != "assistant" && message.Role != "system" && message.Role != "developer" {
			return nil, upstreamError("PROVIDER_BAD_REQUEST")
		}
		content := []any{}
		flush := func() {
			if len(content) > 0 {
				items = append(items, map[string]any{"type": "message", "role": message.Role, "content": content})
				content = nil
			}
		}
		for _, block := range message.Blocks {
			switch block.Kind {
			case "text", "note":
				kind := "input_text"
				if message.Role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]string{"type": kind, "text": block.Text})
			case "artifact":
				if message.Role != "user" && message.Role != "developer" {
					return nil, upstreamError("PROVIDER_BAD_REQUEST")
				}
				parts, err := artifactParts(block.Artifact, request.Model, "responses")
				if err != nil {
					return nil, err
				}
				content = append(content, parts...)
			case "tool_use":
				if block.ToolUse == nil || message.Role != "assistant" || block.ToolUse.ID == "" || block.ToolUse.Name == "" {
					return nil, upstreamError("PROVIDER_BAD_REQUEST")
				}
				arguments, err := json.Marshal(block.ToolUse.Input)
				if err != nil {
					return nil, err
				}
				flush()
				items = append(items, map[string]any{"type": "function_call", "call_id": block.ToolUse.ID, "name": block.ToolUse.Name, "arguments": string(arguments)})
			case "tool_result":
				if block.ToolResult == nil || block.ToolResult.ToolCallID == "" {
					return nil, upstreamError("PROVIDER_BAD_REQUEST")
				}
				flush()
				var output any = block.ToolResult.Content
				if block.ToolResult.Artifact != nil && !block.ToolResult.IsError {
					parts, err := artifactParts(block.ToolResult.Artifact, request.Model, "responses")
					if err != nil {
						return nil, err
					}
					output = append([]any{map[string]string{"type": "input_text", "text": block.ToolResult.Content}}, parts...)
				}
				items = append(items, map[string]any{"type": "function_call_output", "call_id": block.ToolResult.ToolCallID, "output": output})
			case "thinking":
				// 历史思考文本不是可验证的上游推理项，不能伪造 reasoning 事件回传。
			default:
				return nil, domain.NewError(400, "MESSAGE_BLOCK_INVALID", "Unsupported message block.")
			}
		}
		flush()
	}
	body := map[string]any{"model": request.Model.ModelKey, "input": items, "max_output_tokens": request.Model.MaxOutput, "stream": true, "store": false}
	if request.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": request.ReasoningEffort}
	}
	if request.System != "" {
		body["instructions"] = request.System
	}
	if len(request.Tools) > 0 {
		tools := []any{}
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "strict": false})
		}
		body["tools"] = tools
		if request.ToolChoice != "" {
			body["tool_choice"] = request.ToolChoice
		}
	}
	return json.Marshal(body)
}

type responseItem struct {
	ID, Kind  string
	ToolIndex int
	Done      bool
}

// responses 将语义 SSE 转换成执行器事件；完整结束、工具分片与用量均需显式验证。
func (state *streamState) responses(payload []byte, emit func(Event) error) error {
	var event struct {
		Type        string `json:"type"`
		Sequence    *int   `json:"sequence_number"`
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		Delta       string `json:"delta"`
		Arguments   string `json:"arguments"`
		Item        struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		Response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Usage  *struct {
				Input   *int `json:"input_tokens"`
				Output  *int `json:"output_tokens"`
				Details struct {
					Cached *int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Sequence == nil || *event.Sequence < 0 || state.responseSequence != nil && *event.Sequence <= *state.responseSequence || state.done {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	state.responseSequence = event.Sequence
	if event.Type == "error" {
		return upstreamError("PROVIDER_UNAVAILABLE")
	}
	if event.Type == "response.created" {
		if state.responseID != "" || event.Response.ID == "" || event.Response.Status != "in_progress" {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		state.responseID = event.Response.ID
		state.responseItems = map[int]*responseItem{}
		return nil
	}
	if state.responseID == "" {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	if event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed" {
		if event.Response.ID != state.responseID {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		if usage := event.Response.Usage; usage != nil {
			if invalidCount(usage.Input, usage.Output, usage.Details.Cached) || usage.Input != nil && usage.Details.Cached != nil && *usage.Details.Cached > *usage.Input {
				return upstreamError("PROVIDER_INVALID_USAGE")
			}
			state.usage.Reported = &ReportedUsage{InputTokens: usage.Input, OutputTokens: usage.Output, CacheReadTokens: usage.Details.Cached, InputIncludesCache: true}
			if usage.Input != nil && usage.Output != nil {
				cached := 0
				if usage.Details.Cached != nil {
					cached = *usage.Details.Cached
				}
				state.usage.InputTokens, state.usage.OutputTokens, state.usage.CacheReadTokens, state.usage.Estimated = *usage.Input-cached, *usage.Output, cached, false
			}
		}
		if event.Type != "response.completed" || event.Response.Status != "completed" {
			return upstreamError("PROVIDER_RESPONSE_INCOMPLETE")
		}
		for _, item := range state.responseItems {
			if !item.Done {
				return upstreamError("PROVIDER_INVALID_STREAM")
			}
		}
		reason := "stop"
		if len(state.tools) > 0 {
			reason = "tool_calls"
		}
		if err := state.finish(reason, emit); err != nil {
			return err
		}
		state.done = true
		return nil
	}
	if event.Type == "response.in_progress" {
		if event.Response.ID != state.responseID {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		return nil
	}
	if event.OutputIndex < 0 || event.OutputIndex >= 1024 {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	item := state.responseItems[event.OutputIndex]
	if event.Type == "response.output_item.added" {
		if item != nil || event.Item.ID == "" || len(event.Item.ID) > 256 {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		item = &responseItem{ID: event.Item.ID, Kind: event.Item.Type, ToolIndex: len(state.tools)}
		state.responseItems[event.OutputIndex] = item
		switch item.Kind {
		case "function_call":
			if event.Item.CallID == "" || event.Item.Name == "" || len(state.tools) >= 128 {
				return upstreamError("PROVIDER_INVALID_STREAM")
			}
			return state.emitTool(item.ToolIndex, event.Item.CallID, event.Item.Name, event.Item.Arguments, emit)
		case "message", "reasoning":
			return nil
		default:
			return upstreamError("PROVIDER_UNSUPPORTED_OUTPUT")
		}
	}
	if item == nil || item.Done {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	if event.Type == "response.output_item.done" {
		if event.Item.ID != item.ID || event.Item.Type != item.Kind {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		if item.Kind == "function_call" {
			tool := state.tools[item.ToolIndex]
			if event.Item.CallID != tool.ID || event.Item.Name != tool.Name || event.Item.Arguments != tool.Arguments.String() {
				return upstreamError("PROVIDER_INVALID_STREAM")
			}
			if err := state.endTool(item.ToolIndex, emit); err != nil {
				return err
			}
		}
		item.Done = true
		return nil
	}
	if event.ItemID != item.ID {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	switch event.Type {
	case "response.output_text.delta", "response.refusal.delta":
		if item.Kind != "message" {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		state.characters += len([]rune(event.Delta))
		return emit(Event{Kind: "text_delta", Text: event.Delta})
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if item.Kind != "reasoning" {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		return emit(Event{Kind: "thinking_delta", Text: event.Delta})
	case "response.function_call_arguments.delta":
		if item.Kind != "function_call" || state.tools[item.ToolIndex].Ended {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		return state.emitTool(item.ToolIndex, "", "", event.Delta, emit)
	case "response.function_call_arguments.done":
		if item.Kind != "function_call" || state.tools[item.ToolIndex].Ended || event.Arguments != state.tools[item.ToolIndex].Arguments.String() {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		return state.endTool(item.ToolIndex, emit)
	case "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.refusal.done", "response.reasoning_text.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done":
		return nil
	default:
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
}
