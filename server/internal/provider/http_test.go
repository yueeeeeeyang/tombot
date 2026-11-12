package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/testkit"
)

func emitFixture(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			t.Error(err)
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}
func minimalRequest() Request {
	return Request{Model: domain.Model{ModelKey: "fixture-model", MaxOutput: 100}, System: "You are a test assistant.", Messages: []Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Hello"}}}}}
}

func TestOpenAIFragmentedToolsAndUsage(t *testing.T) {
	credential := testkit.Random(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("上游路径或鉴权错误")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["stream"] != true || body["max_completion_tokens"] != float64(100) {
			t.Error("流式请求错误")
		}
		emitFixture(t, w,
			`{"choices":[{"index":0,"delta":{"content":"你好","tool_calls":[{"index":0,"id":"call-a","function":{"name":"file_read","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"notes.txt\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":5}}}`, `[DONE]`)
	}))
	defer server.Close()
	client := NewHTTP(domain.Provider{Kind: "openai_compat", BaseURL: server.URL + "/v1", IsEnabled: true}, []byte(credential))
	events := []Event{}
	usage, err := client.Stream(context.Background(), minimalRequest(), func(event Event) error { events = append(events, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if usage.Estimated || usage.InputTokens != 15 || usage.CacheReadTokens != 5 || usage.OutputTokens != 9 {
		t.Fatalf("用量归一化错误: %+v", usage)
	}
	starts, ends := 0, 0
	for _, event := range events {
		if event.Kind == "tool_use_start" {
			starts++
		}
		if event.Kind == "tool_use_end" {
			ends++
			if event.InputDelta != `{"path":"notes.txt"}` {
				t.Fatal("参数分片合并失败")
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatal("工具事件不成对")
	}
}
func TestAnthropicToolAndThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("anthropic-version") == "" {
			t.Error("Anthropic 请求头错误")
		}
		emitFixture(t, w,
			`{"type":"message_start","message":{"usage":{"input_tokens":8,"cache_read_input_tokens":3}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Consider the task."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-a","name":"file_list","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\".\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`)
	}))
	defer server.Close()
	client := NewHTTP(domain.Provider{Kind: "anthropic", BaseURL: server.URL + "/v1", IsEnabled: true}, nil)
	thinking := false
	usage, err := client.Stream(context.Background(), minimalRequest(), func(event Event) error {
		if event.Kind == "thinking_delta" {
			thinking = true
		}
		return nil
	})
	if err != nil || !thinking || usage.InputTokens != 8 || usage.CacheReadTokens != 3 || usage.OutputTokens != 5 {
		t.Fatalf("Anthropic 事件错误: %+v %v", usage, err)
	}
}
func TestProviderRejectsInterruptedAndMalformedStreams(t *testing.T) {
	for _, frames := range [][]string{
		{`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`},
		{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{`not-json`, `[DONE]`},
		{`[DONE]`},
		{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"read","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`, `[DONE]`},
	} {
		t.Run(fmt.Sprint(frames), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); emitFixture(t, w, frames...) }))
			defer server.Close()
			client := NewHTTP(domain.Provider{Kind: "openai_compat", BaseURL: server.URL, IsEnabled: true}, nil)
			if _, err := client.Stream(context.Background(), minimalRequest(), func(Event) error { return nil }); err == nil {
				t.Fatal("不完整流被接受")
			}
			if calls.Load() != 1 {
				t.Fatal("部分流不应重试")
			}
		})
	}
}
func TestProviderNeverReplaysGenerationAndCancellation(t *testing.T) {
	for _, status := range []int{400, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := NewHTTP(domain.Provider{Kind: "openai_compat", BaseURL: server.URL, IsEnabled: true}, nil)
			if _, err := client.Stream(context.Background(), minimalRequest(), func(Event) error { return nil }); err == nil {
				t.Fatal("上游错误被掩盖")
			}
			if calls.Load() != 1 {
				t.Fatalf("结果不确定的生成请求被重放: %d", calls.Load())
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewHTTP(domain.Provider{Kind: "openai_compat", BaseURL: "http://127.0.0.1:1", IsEnabled: true}, nil)
	if _, err := client.Stream(ctx, minimalRequest(), func(Event) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未传播: %v", err)
	}
}
func TestProviderCallbackFailureStopsWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		emitFixture(t, w, `{"choices":[{"index":0,"delta":{"content":"hello"}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer server.Close()
	client := NewHTTP(domain.Provider{Kind: "openai_compat", BaseURL: server.URL, IsEnabled: true}, nil)
	sentinel := errors.New("测试回调失败")
	if _, err := client.Stream(context.Background(), minimalRequest(), func(Event) error { return sentinel }); !errors.Is(err, sentinel) || calls.Load() != 1 {
		t.Fatal("回调错误未传播或发生重试")
	}
}
func TestProviderDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer source.Close()
	client := NewHTTP(domain.Provider{Kind: "anthropic", BaseURL: source.URL, IsEnabled: true}, []byte(testkit.Random(t)))
	if _, err := client.Stream(context.Background(), minimalRequest(), func(Event) error { return nil }); err == nil || forwarded.Load() != 0 {
		t.Fatal("上游重定向泄漏凭据")
	}
}
