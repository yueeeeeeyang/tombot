package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestConversationHTTPQuestionSearchReadAndToolResults(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) < 2 {
			t.Error("模型请求缺失", err)
			http.Error(w, "invalid request", 400)
			return
		}
		step := calls.Add(1)
		name := "ask_user"
		arguments := map[string]any{"question": "采用哪种格式？", "options": []string{"简报", "表格"}}
		last := input.Messages[len(input.Messages)-1]
		if step > 1 && last.Role != "tool" {
			t.Error("工具结果未回灌")
			http.Error(w, "missing tool result", 400)
			return
		}
		switch step {
		case 2:
			if !strings.Contains(last.Content, "USER_QUESTION_REMOVED") {
				t.Error("提问未立即返回已移除", last.Content)
			}
			name, arguments = "conversation_search", map[string]any{"query": "公开核验"}
		case 3:
			var hits []domain.ConversationHit
			if err := json.Unmarshal([]byte(last.Content), &hits); err != nil || len(hits) != 1 {
				t.Error("会话检索错误", last.Content, err)
				http.Error(w, "invalid transcript search", 400)
				return
			}
			name, arguments = "conversation_read", map[string]any{"conversation_id": hits[0].ConversationID, "around_message_id": hits[0].MessageID, "limit": 3}
		case 4:
			var transcript domain.ConversationTranscript
			if err := json.Unmarshal([]byte(last.Content), &transcript); err != nil || len(transcript.Messages) == 0 || !strings.Contains(transcript.Messages[0].Text, "公开核验") || strings.Contains(last.Content, "采用哪种格式") {
				t.Error("公开转录错误或泄漏工具参数", last.Content, err)
			}
		}
		var delta map[string]any
		reason := "tool_calls"
		if step <= 3 {
			encoded, err := json.Marshal(arguments)
			if err != nil {
				t.Error(err)
				return
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "interaction", "type": "function", "function": map[string]string{"name": name, "arguments": string(encoded)}}}}
		} else {
			reason, delta = "stop", map[string]any{"content": "已按回答完成公开资料核验。"}
		}
		encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 10}})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := server.Auth.Login(ctx, password, "会话提问测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	spec, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "会话模型", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price, supports := int64(1000000), true
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "fixture", DisplayName: "会话模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, SupportsTools: &supports, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher", ToolPolicy: &domain.ToolPolicy{AllowedTools: []string{"ask_user", "conversation_search", "conversation_read"}, Approval: "standard"}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	response := request(t, handler, "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "公开核验资料，并确认我的输出偏好。"}, headers)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	var sent agent.Submission
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		wake := server.Bus.Wake()
		run, err := server.Runtime.Run(ctx, sent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "succeeded" {
			if calls.Load() != 4 || run.CostMicro != 40 {
				t.Fatal("工具循环或费用不完整", run, calls.Load())
			}
			break
		}
		if run.Status == "failed" || run.Status == "canceled" {
			t.Fatal(run)
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatal("会话工具链未完成")
		}
	}
}
