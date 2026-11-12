package provider

import (
	"encoding/json"
	"sort"
	"strings"
)

type toolState struct {
	ID, Name       string
	Arguments      strings.Builder
	Started, Ended bool
}
type streamState struct {
	tools      map[int]*toolState
	usage      Usage
	done       bool
	finished   bool
	characters int
}

func (s *streamState) emitTool(index int, id, name, delta string, emit func(Event) error) error {
	tool := s.tools[index]
	if tool == nil {
		tool = &toolState{}
		s.tools[index] = tool
	}
	if id != "" {
		tool.ID = id
	}
	if name != "" {
		tool.Name += name
	}
	if !tool.Started && tool.ID != "" && tool.Name != "" {
		if err := emit(Event{Kind: "tool_use_start", Index: index, ToolCallID: tool.ID, ToolName: tool.Name}); err != nil {
			return err
		}
		tool.Started = true
	}
	if delta != "" {
		tool.Arguments.WriteString(delta)
		s.characters += len([]rune(delta))
		if !tool.Started {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		return emit(Event{Kind: "tool_use_delta", Index: index, ToolCallID: tool.ID, InputDelta: delta})
	}
	return nil
}
func (s *streamState) endTool(index int, emit func(Event) error) error {
	tool := s.tools[index]
	if tool == nil || tool.Ended {
		return nil
	}
	if !tool.Started {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	arguments := tool.Arguments.String()
	if arguments == "" {
		arguments = "{}"
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return upstreamError("PROVIDER_INVALID_TOOL_INPUT")
	}
	tool.Ended = true
	return emit(Event{Kind: "tool_use_end", Index: index, ToolCallID: tool.ID, ToolName: tool.Name, InputDelta: arguments})
}
func (s *streamState) finish(reason string, emit func(Event) error) error {
	if s.finished {
		return nil
	}
	indices := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if err := s.endTool(index, emit); err != nil {
			return err
		}
	}
	s.finished = true
	return emit(Event{Kind: "stop", StopReason: reason})
}

func (s *streamState) openAI(payload string, emit func(Event) error) error {
	if payload == "[DONE]" {
		if !s.finished {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		s.done = true
		return nil
	}
	var chunk struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Input   *int `json:"prompt_tokens"`
			Output  *int `json:"completion_tokens"`
			Details struct {
				Cached *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return upstreamError("PROVIDER_UNAVAILABLE")
	}
	if chunk.Usage != nil {
		if invalidCount(chunk.Usage.Input, chunk.Usage.Output, chunk.Usage.Details.Cached) ||
			(chunk.Usage.Input != nil && chunk.Usage.Details.Cached != nil && *chunk.Usage.Details.Cached > *chunk.Usage.Input) {
			return upstreamError("PROVIDER_INVALID_USAGE")
		}
		s.usage.Reported = &ReportedUsage{InputTokens: chunk.Usage.Input, OutputTokens: chunk.Usage.Output, CacheReadTokens: chunk.Usage.Details.Cached, InputIncludesCache: true}
		if chunk.Usage.Input != nil && chunk.Usage.Output != nil {
			cached := 0
			if chunk.Usage.Details.Cached != nil {
				cached = *chunk.Usage.Details.Cached
			}
			s.usage.InputTokens = *chunk.Usage.Input - cached
			s.usage.OutputTokens = *chunk.Usage.Output
			s.usage.CacheReadTokens = cached
			s.usage.Estimated = false
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if choice.Delta.Content != "" {
			s.characters += len([]rune(choice.Delta.Content))
			if err := emit(Event{Kind: "text_delta", Text: choice.Delta.Content}); err != nil {
				return err
			}
		}
		if choice.Delta.Reasoning != "" {
			s.characters += len([]rune(choice.Delta.Reasoning))
			if err := emit(Event{Kind: "thinking_delta", Text: choice.Delta.Reasoning}); err != nil {
				return err
			}
		}
		for _, tool := range choice.Delta.ToolCalls {
			if err := s.emitTool(tool.Index, tool.ID, tool.Function.Name, tool.Function.Arguments, emit); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			if err := s.finish(*choice.FinishReason, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) anthropic(payload []byte, emit func(Event) error) error {
	var chunk struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage struct {
				Input  *int `json:"input_tokens"`
				Output *int `json:"output_tokens"`
				Cache  *int `json:"cache_read_input_tokens"`
				Write  *int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Block struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return upstreamError("PROVIDER_INVALID_STREAM")
	}
	switch chunk.Type {
	case "message_start":
		if invalidCount(chunk.Message.Usage.Input, chunk.Message.Usage.Output, chunk.Message.Usage.Cache, chunk.Message.Usage.Write) {
			return upstreamError("PROVIDER_INVALID_USAGE")
		}
		s.usage.Reported = &ReportedUsage{InputTokens: chunk.Message.Usage.Input, CacheReadTokens: chunk.Message.Usage.Cache, CacheWriteTokens: chunk.Message.Usage.Write}
		if chunk.Message.Usage.Input != nil {
			s.usage.InputTokens = *chunk.Message.Usage.Input
		}
		if chunk.Message.Usage.Cache != nil {
			s.usage.CacheReadTokens = *chunk.Message.Usage.Cache
		}
	case "content_block_start":
		if chunk.Block.Type == "tool_use" {
			delta := ""
			if len(chunk.Block.Input) > 0 && string(chunk.Block.Input) != "{}" {
				delta = string(chunk.Block.Input)
			}
			return s.emitTool(chunk.Index, chunk.Block.ID, chunk.Block.Name, delta, emit)
		}
		if chunk.Block.Text != "" {
			s.characters += len([]rune(chunk.Block.Text))
			return emit(Event{Kind: "text_delta", Index: chunk.Index, Text: chunk.Block.Text})
		}
	case "content_block_delta":
		switch chunk.Delta.Type {
		case "text_delta":
			s.characters += len([]rune(chunk.Delta.Text))
			return emit(Event{Kind: "text_delta", Index: chunk.Index, Text: chunk.Delta.Text})
		case "thinking_delta":
			s.characters += len([]rune(chunk.Delta.Thinking))
			return emit(Event{Kind: "thinking_delta", Index: chunk.Index, Text: chunk.Delta.Thinking})
		case "input_json_delta":
			return s.emitTool(chunk.Index, "", "", chunk.Delta.PartialJSON, emit)
		}
	case "content_block_stop":
		return s.endTool(chunk.Index, emit)
	case "message_delta":
		if invalidCount(chunk.Usage.Output) {
			return upstreamError("PROVIDER_INVALID_USAGE")
		}
		if chunk.Usage.Output != nil {
			if s.usage.Reported == nil {
				s.usage.Reported = &ReportedUsage{}
			}
			s.usage.Reported.OutputTokens = chunk.Usage.Output
			s.usage.OutputTokens = *chunk.Usage.Output
			s.usage.Estimated = s.usage.Reported.InputTokens == nil
		}
		if chunk.Delta.StopReason != "" {
			return s.finish(chunk.Delta.StopReason, emit)
		}
	case "message_stop":
		if !s.finished {
			return upstreamError("PROVIDER_INVALID_STREAM")
		}
		s.done = true
	case "error":
		return upstreamError("PROVIDER_UNAVAILABLE")
	}
	return nil
}

func invalidCount(counts ...*int) bool {
	for _, count := range counts {
		if count != nil && *count < 0 {
			return true
		}
	}
	return false
}
