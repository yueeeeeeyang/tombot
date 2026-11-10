package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

// localModelFixture 仅允许显式配置的环回推理端点，返回独立实例与临时设备凭据。
func localModelFixture(t *testing.T, ctx context.Context, maxOutput int) (*Server, string) {
	t.Helper()
	address, modelKey := os.Getenv("TOM_TEST_LOCAL_MODEL_URL"), os.Getenv("TOM_TEST_LOCAL_MODEL")
	if address == "" && modelKey == "" {
		t.Skip("未配置本地真实模型")
	}
	endpoint, err := url.Parse(address)
	if err != nil || endpoint.Scheme != "http" || !net.ParseIP(endpoint.Hostname()).IsLoopback() || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || strings.TrimSpace(modelKey) == "" {
		t.Fatal("请提供环回 HTTP 模型地址和模型标识")
	}
	server, setup := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, setup); err != nil {
		t.Fatal(err)
	}
	platform := "macos"
	if strings.HasPrefix(os.Getenv("TOM_TEST_BOT_DEVICE"), "emulator-") {
		platform = "android"
	}
	session, err := server.Auth.Login(ctx, password, "本地真实模型验收", platform, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "本地推理", BaseURL: address}, "user")
	if err != nil {
		t.Fatal(err)
	}
	noTools := false
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: p.ID, ModelKey: modelKey, DisplayName: "本地模型", ContextWindow: 4096, MaxOutput: maxOutput, IsDefault: true, SupportsTools: &noTools}, "user"); err != nil {
		t.Fatal(err)
	}
	return server, session.DeviceToken
}

// TestLocalModelChatStreaming 复核真实权重推理的流式增量与最终消息。
func TestLocalModelChatStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// 长回复需覆盖原生输入法收起与下一帧，才能实际点击中止并核对已显示的部分正文。
	server, device := localModelFixture(t, ctx, 768)
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "assistant"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(server.Handler())
	defer host.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host.URL+"/api/v1/events?conversation_id="+bot.ConversationID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+device)
	stream, err := host.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if stream.StatusCode != http.StatusOK {
		t.Fatal("会话事件流未打开", stream.StatusCode)
	}
	started := time.Now()
	response := request(t, server.Handler(), "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "What is 7 plus 5? Answer using just the number in digits."}, map[string]string{"Authorization": "Bearer " + device})
	if response.Code != 202 {
		t.Fatal("消息未受理", response.Code)
	}
	var sent agent.Submission
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(stream.Body)
	var kind string
	var text strings.Builder
	var first time.Duration
	finished := false
	for !finished && scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "event: ") {
			kind = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload struct {
			Text   string
			TurnID string `json:"turn_id"`
			Status string
		}
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "text.delta":
			if text.Len() == 0 && payload.Text != "" {
				first = time.Since(started)
			}
			text.WriteString(payload.Text)
		case "turn.finished":
			if payload.TurnID != sent.TurnID || payload.Status != "succeeded" {
				t.Fatal("真实推理未正常收尾", payload.Status)
			}
			finished = true
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if !finished || !strings.Contains(text.String(), "12") {
		t.Fatal("未从模型增量收到预期的算术结果")
	}
	snapshot, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
	if err != nil || snapshot.ActiveTurnID != nil || len(snapshot.Messages) != 2 {
		t.Fatal("真实推理的收尾快照不完整", err)
	}
	reply := snapshot.Messages[1]
	var final strings.Builder
	for _, block := range reply.Blocks {
		if block.Kind == "text" {
			final.WriteString(block.Text)
		}
	}
	if reply.Status != "complete" || final.String() != text.String() || reply.TokenIn <= 0 || reply.TokenOut <= 0 {
		t.Fatal("流式回复、最终文本或用量不一致", reply.TokenIn, reply.TokenOut)
	}
	t.Logf("真实模型流式回答通过，首段 %s，总计 %s，输入 %d、输出 %d token", first, time.Since(started), reply.TokenIn, reply.TokenOut)
	if os.Getenv("TOM_TEST_NATIVE_MODEL") == "1" {
		verifyNativeModelChat(t, server, bot, device)
	}
}
