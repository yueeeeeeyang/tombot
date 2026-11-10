package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/testkit"
)

func TestDeepSeekDiscoveryReadsRealMetadataAndAllowsOmittedLimits(t *testing.T) {
	credential := testkit.Random(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("发现接口路径或认证错误")
		}
		if _, err := fmt.Fprint(w, `{"object":"list","data":[{"id":"new-unlisted-model","object":"model","name":"新模型","context_window":65536,"max_output_tokens":8192,"input_modalities":["text","image"]},{"id":"metadata-optional","object":"model"}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	models, err := DiscoverDeepSeek(context.Background(), server.URL+"/v1", []byte(credential), time.Second)
	if err != nil || len(models) != 2 || models[0].ID != "new-unlisted-model" || *models[0].ContextWindow != 65536 || models[1].ContextWindow != nil || models[1].Name != "metadata-optional" || calls.Load() != 1 {
		t.Fatal("模型发现未遵循真实响应", err)
	}
}

func TestDeepSeekDiscoveryRejectsInvalidAndOversizedResponsesWithoutLeaking(t *testing.T) {
	for name, body := range map[string]string{
		"空列表":   `{"object":"list","data":[]}`,
		"空模型标识": `{"object":"list","data":[{"object":"model"}]}`,
		"重复模型":  `{"object":"list","data":[{"id":"same","object":"model"},{"id":"same","object":"model"}]}`,
		"非法规格":  `{"object":"list","data":[{"id":"model","object":"model","context_window":0}]}`,
		"超出上下文": `{"object":"list","data":[{"id":"model","object":"model","context_window":1024,"max_output_tokens":2048}]}`,
		"不支持文本": `{"object":"list","data":[{"id":"model","object":"model","input_modalities":["audio"]}]}`,
		"非JSON": "invalid upstream response",
		"超限响应":  strings.Repeat(" ", maxDiscoveryBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, err := DiscoverDeepSeek(context.Background(), server.URL, []byte(testkit.Random(t)), time.Second)
			var failure *domain.Error
			if !errors.As(err, &failure) || failure.Code != "PROVIDER_MODELS_INVALID" || strings.Contains(err.Error(), "upstream response") {
				t.Fatal("非法发现响应未安全拒绝", err)
			}
		})
	}
}

func TestDeepSeekDiscoveryBoundsFailuresAndDoesNotFollowRedirects(t *testing.T) {
	credential := testkit.Random(t)
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	for _, status := range []int{401, 403, 429, 500, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				if _, err := fmt.Fprint(w, credential); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, err := DiscoverDeepSeek(context.Background(), server.URL, []byte(credential), time.Second)
			if err == nil || strings.Contains(err.Error(), credential) || calls.Load() != 1 || forwarded.Load() != 0 {
				t.Fatal("失败泄露密钥或发生重试、重定向")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	_, err := DiscoverDeepSeek(context.Background(), server.URL, []byte(credential), 20*time.Millisecond)
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "PROVIDER_TIMEOUT" {
		t.Fatal("发现超时未明确报告", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = DiscoverDeepSeek(canceled, server.URL, []byte(credential), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal("取消丢失", err)
	}
}

func TestDeepSeekMultiTurnToolsUseOfficialRequestContract(t *testing.T) {
	client := NewHTTP(domain.Provider{Kind: "openai_compat", Preset: "deepseek", IsEnabled: true}, nil)
	request := minimalRequest()
	botID := "call-tool"
	request.Messages = append(request.Messages, Message{Role: "assistant", Blocks: []domain.MessageBlock{{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: botID, Name: "file_read", Input: map[string]string{"path": "notes/a.txt"}}}}}, Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: botID, Content: "contents"}}}})
	encoded, err := client.encode(request)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["max_tokens"] != float64(100) || body["max_completion_tokens"] != nil || body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatal("DeepSeek 请求参数不符合官方协议")
	}
	messages := body["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != botID {
		t.Fatal("多轮工具结果顺序不完整")
	}
}

func TestDeepSeekDiscoveryRejectsTooManyModelsAndBoundsBodyRead(t *testing.T) {
	models := make([]map[string]string, maxDiscoveredModels+1)
	for index := range models {
		models[index] = map[string]string{"id": fmt.Sprintf("model-%d", index), "object": "model"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, err := DiscoverDeepSeek(context.Background(), server.URL, []byte(testkit.Random(t)), time.Second)
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "PROVIDER_MODELS_INVALID" {
		t.Fatal("接受了超出数量上限的模型列表", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer slow.Close()
	_, err = DiscoverDeepSeek(context.Background(), slow.URL, []byte(testkit.Random(t)), 20*time.Millisecond)
	if !errors.As(err, &failure) || failure.Code != "PROVIDER_TIMEOUT" {
		t.Fatal("响应正文读取未受超时限制", err)
	}
}
