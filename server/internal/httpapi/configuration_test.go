package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestConfigurationBotFlowAndProviderProbe(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "测试客户端", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		if err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	response := request(t, handler, "POST", "/api/v1/providers", service.ProviderInput{Kind: "openai_compat", DisplayName: "测试供应商", BaseURL: upstream.URL + "/v1"}, headers)
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var p domain.Provider
	if err = json.Unmarshal(response.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "POST", "/api/v1/models", service.ModelInput{ProviderID: p.ID, ModelKey: "test-model", DisplayName: "测试模型", ContextWindow: 4096, MaxOutput: 100, IsDefault: true}, headers)
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var model domain.Model
	if err = json.Unmarshal(response.Body.Bytes(), &model); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "POST", "/api/v1/providers/"+p.ID+"/test", map[string]any{}, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "POST", "/api/v1/bots", service.BotInput{Template: "researcher"}, headers)
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var bot domain.Bot
	if err = json.Unmarshal(response.Body.Bytes(), &bot); err != nil {
		t.Fatal(err)
	}
	if bot.ModelID == nil || *bot.ModelID != model.ID {
		t.Fatal("Bot 未继承默认模型")
	}
	response = request(t, handler, "DELETE", "/api/v1/models/"+model.ID, nil, headers)
	if response.Code != 409 {
		t.Fatal("引用模型未被保护")
	}
	response = request(t, handler, "GET", "/api/v1/conversations?limit=1", nil, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var page service.ConversationPage
	if err = json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != bot.ConversationID {
		t.Fatal("左栏未取得 Bot 会话")
	}
	response = request(t, handler, "GET", "/api/v1/bots?limit=0", nil, headers)
	if response.Code != 400 {
		t.Fatal("无效分页未被拒绝")
	}
	response = request(t, handler, "GET", "/api/v1/providers", nil, nil)
	if response.Code != 401 {
		t.Fatal("供应商信息未受鉴权保护")
	}
}
