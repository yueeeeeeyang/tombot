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

func TestHTTPIndependentReviewApprovalExecutionAndCost(t *testing.T) {
	var executionCalls, reviewCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string            `json:"model"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		delta := map[string]any{}
		reason := "stop"
		input, output := 2, 1
		switch req.Model {
		case "reviewer":
			reviewCalls.Add(1)
			if len(req.Tools) != 0 || !strings.Contains(string(req.Messages[len(req.Messages)-1].Content), "请生成报告") {
				t.Error("审查请求缺少意图或错误启用了工具")
			}
			delta["content"] = `{"consistent":true,"risk":"high","reason":"请确认报告内容和写入目标。"}`
			input, output = 4, 2
		case "executor":
			if executionCalls.Add(1) == 1 {
				reason = "tool_calls"
				delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "write-report", "type": "function", "function": map[string]any{"name": "file_write", "arguments": `{"path":"out/report.md","content":"报告内容"}`}}}
			} else {
				last := req.Messages[len(req.Messages)-1]
				if last.Role != "tool" || !strings.Contains(string(last.Content), "sha256") {
					t.Error("执行模型没有收到已完成的工具结果")
				}
				delta["content"] = "报告已生成。"
			}
		default:
			t.Error("调用了未知模型")
			http.Error(w, "invalid", 400)
			return
		}
		chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": input, "completion_tokens": output}})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := server.Auth.Login(ctx, password, "独立审查验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	spec, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "本地验收", BaseURL: upstream.URL}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	_, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "executor", DisplayName: "执行模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price = 2_000_000
	reviewer, err := server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: spec.ID, ModelKey: "reviewer", DisplayName: "审查模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price}, "user")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "writer", ToolPolicy: &domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"file_write"}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	response := request(t, handler, "PATCH", "/api/v1/settings", map[string]any{"reviewer": map[string]any{"enabled": true}}, headers)
	if response.Code != 200 {
		t.Fatal("无法启用审查", response.Code)
	}
	response = request(t, handler, "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "请生成报告"}, headers)
	var sent agent.Submission
	if response.Code != 202 {
		t.Fatal("运行提交失败", response.Code)
	}
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	for {
		wake := server.Bus.Wake()
		run, err := server.Runtime.Run(ctx, sent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "succeeded" {
			if run.CostMicro != 6 {
				t.Fatal("执行费用不正确", run.CostMicro)
			}
			break
		}
		if run.Status == "failed" {
			t.Fatal("执行失败", run.ErrorCode)
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("审批后未完成")
		}
	}
	if reviewCalls.Load() != 0 || executionCalls.Load() != 2 {
		t.Fatal("工具执行仍调用了审查模型", reviewCalls.Load(), executionCalls.Load())
	}
	file, err := server.Files.Read(ctx, bot.ID, "bot", "out/report.md")
	if err != nil || !strings.Contains(file.Content, "报告内容") {
		t.Fatal("工具未立即生成文件", err)
	}
	if reviewer.ID == "" {
		t.Fatal("审查模型未创建")
	}
}
