package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

// toolChatFixture 为协议验收创建临时供应商、标准审批 Bot 和设备会话。
func toolChatFixture(t *testing.T, address string, tools []string) (*Server, domain.Bot, string) {
	t.Helper()
	server, code := fixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "工具协作验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "本地工具测试", BaseURL: address + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: p.ID, ModelKey: "fixture", DisplayName: "工具模型", ContextWindow: 8192, MaxOutput: 1024, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "assistant", ToolPolicy: &domain.ToolPolicy{AllowedTools: tools, Approval: "standard"}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	return server, bot, session.DeviceToken
}

// writeToolChatReply 生成标准流式增量，使测试动作经过真实适配、调度、审批和消息投影。
func writeToolChatReply(t *testing.T, w http.ResponseWriter, text string, actions ...domain.ToolUseBlock) {
	t.Helper()
	var calls []map[string]any
	for i, action := range actions {
		body, err := json.Marshal(action.Input)
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid arguments", 500)
			return
		}
		calls = append(calls, map[string]any{"index": i, "id": action.ID, "type": "function", "function": map[string]string{"name": action.Name, "arguments": string(body)}})
	}
	delta, reason := map[string]any{"content": text}, "stop"
	if len(calls) > 0 {
		delta, reason = map[string]any{"tool_calls": calls}, "tool_calls"
	}
	chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
	if err != nil {
		t.Error(err)
		http.Error(w, "invalid fixture", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk); err != nil {
		t.Error(err)
	}
}
