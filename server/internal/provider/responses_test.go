package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/testkit"
)

// responseFrames 给测试事件补入真实协议要求的递增序号，不依赖生产编码器生成预期结果。
func responseFrames(t *testing.T, events ...string) []string {
	t.Helper()
	result := make([]string, len(events))
	for index, event := range events {
		var value map[string]any
		if json.Unmarshal([]byte(event), &value) != nil {
			t.Fatal("测试事件格式错误")
		}
		value["sequence_number"] = index
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		result[index] = string(encoded)
	}
	return result
}

func TestResponsesStreamingToolsAndUsage(t *testing.T) {
	credential := testkit.Random(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+credential || r.Header.Get("anthropic-version") != "" {
			t.Error("Responses 请求端点或认证错误")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["stream"]) != "true" || string(body["store"]) != "false" || body["max_output_tokens"] == nil || body["messages"] != nil {
			t.Error("Responses 请求字段错误")
		}
		emitFixture(t, w, responseFrames(t,
			`{"type":"response.created","response":{"id":"response-a","status":"in_progress"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"message-a","type":"message"}}`,
			`{"type":"response.output_text.delta","output_index":0,"item_id":"message-a","delta":"已收到"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"message-a","type":"message"}}`,
			`{"type":"response.output_item.added","output_index":4,"item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":4,"item_id":"item-a","delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":4,"item_id":"item-a","delta":"\"notes.txt\"}"}`,
			`{"type":"response.function_call_arguments.done","output_index":4,"item_id":"item-a","arguments":"{\"path\":\"notes.txt\"}"}`,
			`{"type":"response.output_item.done","output_index":4,"item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"read_file","arguments":"{\"path\":\"notes.txt\"}"}}`,
			`{"type":"response.completed","response":{"id":"response-a","status":"completed","usage":{"input_tokens":20,"output_tokens":9,"input_tokens_details":{"cached_tokens":5}}}}`,
		)...)
	}))
	defer server.Close()
	adapter := NewHTTP(domain.Provider{Kind: "responses", BaseURL: server.URL + "/v1", IsEnabled: true}, []byte(credential))
	var events []Event
	usage, err := adapter.Stream(context.Background(), minimalRequest(), func(event Event) error { events = append(events, event); return nil })
	if err != nil || usage.Estimated || usage.InputTokens != 15 || usage.OutputTokens != 9 || usage.CacheReadTokens != 5 || usage.Reported == nil || !usage.Reported.InputIncludesCache {
		t.Fatal("Responses 流或真实用量错误", err)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		if strings.HasPrefix(event.Kind, "tool_use") && (event.Index != 0 || event.ToolCallID != "call-a") {
			t.Fatal("工具索引或调用标识错误")
		}
	}
	if !reflect.DeepEqual(kinds, []string{"text_delta", "tool_use_start", "tool_use_delta", "tool_use_delta", "tool_use_end", "stop"}) || events[len(events)-1].StopReason != "tool_calls" {
		t.Fatal("Responses 工具事件缺失或重复", kinds)
	}
}

func TestResponsesEncodeHistoryAndTools(t *testing.T) {
	request := minimalRequest()
	request.Tools = []ToolSchema{{Name: "read_file", Description: "Read a file.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}}
	request.ToolChoice = "required"
	request.Messages = []Message{
		{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Read notes."}}},
		{Role: "assistant", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Checking."}, {Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: "call-a", Name: "read_file", Input: map[string]any{"path": "notes.txt"}}}}},
		{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "call-a", Content: "file contents"}}}},
	}
	encoded, err := NewHTTP(domain.Provider{Kind: "responses"}, nil).encode(request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Type, Role, CallID, Name, Arguments string
			Content                             []map[string]string
			Output                              string
		}
		Tools      []map[string]any
		ToolChoice string `json:"tool_choice"`
	}
	// 单独解码 call_id，避免 Go 字段名推断掩盖协议拼写错误。
	var wire map[string]json.RawMessage
	if json.Unmarshal(encoded, &wire) != nil || json.Unmarshal(encoded, &body) != nil {
		t.Fatal("请求解码失败")
	}
	if len(body.Input) != 4 || body.Input[1].Content[0]["type"] != "output_text" || body.Input[2].Type != "function_call" || body.Input[3].Type != "function_call_output" || body.Input[3].Output != "file contents" || !strings.Contains(string(wire["input"]), `"call_id":"call-a"`) {
		t.Fatal("工具历史未按 Responses 结构编码")
	}
	if len(body.Tools) != 1 || body.Tools[0]["type"] != "function" || body.Tools[0]["name"] != "read_file" || body.Tools[0]["function"] != nil || body.Tools[0]["strict"] != false || body.ToolChoice != "required" {
		t.Fatal("Responses 工具定义或选择丢失")
	}
}

func TestResponsesRejectsIncompleteAndInvalidStreams(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"response-a","status":"in_progress"}}`
	for _, event := range []string{
		`{"type":"response.incomplete","response":{"id":"response-a","status":"incomplete","usage":{"input_tokens":3,"output_tokens":5}}}`,
		`{"type":"response.failed","response":{"id":"response-a","status":"failed"}}`,
		`{"type":"error","message":"upstream details"}`,
		`{"type":"response.completed","response":{"id":"response-b","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"response-a","status":"completed","usage":{"input_tokens":-1}}}`,
		`{"type":"response.completed","response":{"id":"response-a","status":"completed","usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"missing","delta":"{}"}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item-a","type":"custom_tool_call"}}`,
	} {
		t.Run(event, func(t *testing.T) {
			state := streamState{tools: map[int]*toolState{}, usage: Usage{Estimated: true}}
			frames := responseFrames(t, created, event)
			emit := func(event Event) error {
				if event.Kind == "stop" {
					t.Error("错误响应被标记成功")
				}
				return nil
			}
			if state.responses([]byte(frames[0]), emit) != nil || state.responses([]byte(frames[1]), emit) == nil {
				t.Fatal("非法 Responses 事件未拒绝")
			}
			if strings.Contains(event, "response.incomplete") && (state.usage.Reported == nil || *state.usage.Reported.OutputTokens != 5) {
				t.Fatal("失败丢失真实用量")
			}
		})
	}
	for _, frames := range [][]string{responseFrames(t, created), {`[DONE]`}, {`not-json`}, responseFrames(t, created, created)} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; emitFixture(t, w, frames...) }))
		adapter := NewHTTP(domain.Provider{Kind: "responses", BaseURL: server.URL, IsEnabled: true}, nil)
		_, err := adapter.Stream(context.Background(), minimalRequest(), func(Event) error { return nil })
		server.Close()
		if err == nil || calls != 1 {
			t.Fatal("中断或无效流被接受或重试")
		}
	}
}

func TestResponsesCallbackFailure(t *testing.T) {
	state := streamState{tools: map[int]*toolState{}}
	frames := responseFrames(t, `{"type":"response.created","response":{"id":"response-a","status":"in_progress"}}`, `{"type":"response.output_item.added","output_index":0,"item":{"id":"message-a","type":"message"}}`, `{"type":"response.output_text.delta","output_index":0,"item_id":"message-a","delta":"hello"}`)
	failure := errors.New("测试回调失败")
	for index, frame := range frames {
		err := state.responses([]byte(frame), func(Event) error { return failure })
		if index == 2 && !errors.Is(err, failure) {
			t.Fatal("回调失败未传播")
		}
	}
}
