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

func TestMemoryHTTPModelApprovalWriteSearchAndPromptIndex(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) < 2 {
			t.Error("模型请求无效", err)
			http.Error(w, "invalid request", 400)
			return
		}
		step := calls.Add(1)
		reason := "tool_calls"
		var delta map[string]any
		if step > 1 {
			last := input.Messages[len(input.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "官方文档") || !strings.Contains(input.Messages[0].Content, "研究偏好") {
				t.Error("记忆结果或提示索引未接入", last.Content)
				http.Error(w, "missing memory", 400)
				return
			}
		}
		if step <= 2 {
			name := "memory_write"
			arguments := map[string]any{"title": "研究偏好", "summary": "优先官方文档。", "content": "保留一手来源与交叉核验依据。", "importance": 4}
			if step == 2 {
				name = "memory_search"
				arguments = map[string]any{"query": "官方文档"}
			}
			encoded, err := json.Marshal(arguments)
			if err != nil {
				t.Error(err)
				return
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "memory", "type": "function", "function": map[string]string{"name": name, "arguments": string(encoded)}}}}
		} else {
			reason = "stop"
			delta = map[string]any{"content": "私有偏好已保存并检索确认。"}
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
	session, err := server.Auth.Login(ctx, password, "记忆执行测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	provider, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "记忆模型", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000000)
	supports := true
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: provider.ID, ModelKey: "fixture", DisplayName: "记忆模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, SupportsTools: &supports, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher", ToolPolicy: &domain.ToolPolicy{AllowedTools: []string{"memory_write", "memory_search"}, Approval: "standard"}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server.Handler(), "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "请记录我的信源偏好并检索确认。"}, headers)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	var sent agent.Submission
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	approved := false
	for {
		wake := server.Bus.Wake()
		pending, err := server.Approvals.List(ctx, "pending", "", 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, approval := range pending {
			if approved {
				t.Fatal("无副作用的搜索再次要求标准审批")
			}
			if err = server.Approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
				t.Fatal(err)
			}
			approved = true
		}
		run, err := server.Runtime.Run(ctx, sent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "succeeded" {
			if !approved || run.CostMicro != 30 || calls.Load() != 3 {
				t.Fatal("审批或调用费用错误", approved, run, calls.Load())
			}
			break
		}
		if run.Status == "failed" {
			t.Fatal(run)
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatal("记忆执行未完成")
		}
	}
	entries, err := server.Memories.List(ctx, bot.ID)
	if err != nil || len(entries) != 1 || entries[0].Title != "研究偏好" {
		t.Fatal("记忆保存重复或丢失", entries, err)
	}
}
