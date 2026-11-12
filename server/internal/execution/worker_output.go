package execution

import (
	"encoding/json"
	"io.yueyang/tom/internal/botagent"
	"sort"
	"strings"

	"io.yueyang/tom/internal/provider"
)

type outputCollector struct {
	text                     strings.Builder
	tools                    map[int]*botagent.ToolCall
	ended                    map[int]bool
	toolIDs                  map[string]bool
	allowed                  map[string]Tool
	bytes, maximum, maxTools int
	stop                     string
}

func newCollector(allowed map[string]Tool, maximum, maxTools int) *outputCollector {
	return &outputCollector{tools: map[int]*botagent.ToolCall{}, ended: map[int]bool{}, toolIDs: map[string]bool{}, allowed: allowed, maximum: maximum, maxTools: maxTools}
}
func (collector *outputCollector) emit(event provider.Event) error {
	if collector.stop != "" {
		return botagent.ErrModelOutput
	}
	switch event.Kind {
	case "thinking_delta":
		// 推理片段不进入消息、操作结果或工具上下文，传输层仍限制总响应大小。
		return nil
	case "text_delta":
		if !validText("a"+event.Text, collector.maximum+1) {
			return botagent.ErrModelOutput
		}
		collector.bytes += len(event.Text)
		if collector.bytes > collector.maximum {
			return botagent.ErrModelOutput
		}
		collector.text.WriteString(event.Text)
	case "tool_use_start":
		if event.Index < 0 || event.Index >= collector.maxTools || collector.tools[event.Index] != nil || collector.toolIDs[event.ToolCallID] || !validText(event.ToolCallID, 256) || collector.allowed[event.ToolName] == nil {
			return botagent.ErrModelOutput
		}
		collector.tools[event.Index] = &botagent.ToolCall{ID: event.ToolCallID, Name: event.ToolName}
		collector.toolIDs[event.ToolCallID] = true
	case "tool_use_delta":
		tool := collector.tools[event.Index]
		if tool == nil || collector.ended[event.Index] || tool.ID != event.ToolCallID {
			return botagent.ErrModelOutput
		}
		collector.bytes += len(event.InputDelta)
		if collector.bytes > collector.maximum {
			return botagent.ErrModelOutput
		}
	case "tool_use_end":
		tool := collector.tools[event.Index]
		if tool == nil || collector.ended[event.Index] || tool.ID != event.ToolCallID || tool.Name != event.ToolName || len(event.InputDelta) > collector.maximum {
			return botagent.ErrModelOutput
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(event.InputDelta), &object) != nil || object == nil {
			return botagent.ErrModelOutput
		}
		tool.Arguments = event.InputDelta
		collector.ended[event.Index] = true
	case "stop":
		switch event.StopReason {
		case "stop", "end_turn", "tool_use", "tool_calls", "max_tokens", "length", "stop_sequence":
		default:
			return botagent.ErrModelOutput
		}
		for index := range collector.tools {
			if !collector.ended[index] {
				return botagent.ErrModelOutput
			}
		}
		collector.stop = event.StopReason
	default:
		return botagent.ErrModelOutput
	}
	return nil
}
func (collector *outputCollector) result() (botagent.ModelOutput, error) {
	output := botagent.ModelOutput{Text: collector.text.String(), Tools: []botagent.ToolCall{}, StopReason: collector.stop}
	indices := make([]int, 0, len(collector.tools))
	for index := range collector.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if collector.ended[index] {
			output.Tools = append(output.Tools, *collector.tools[index])
		}
	}
	encoded, err := json.Marshal(output)
	if err != nil || len(encoded) > 56<<10 || collector.stop == "" || strings.TrimSpace(output.Text) == "" && len(output.Tools) == 0 {
		return output, botagent.ErrModelOutput
	}
	return output, nil
}
