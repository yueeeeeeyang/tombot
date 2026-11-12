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

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestProviderConnectHTTPContractAuthenticationAndSafeErrors(t *testing.T) {
	credential := testkit.Random(t)
	var status atomic.Int32
	status.Store(200)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer "+credential {
			t.Error("模型发现请求错误")
		}
		w.WriteHeader(int(status.Load()))
		body := `{"object":"list","data":[{"id":"http-discovered","object":"model","name":"HTTP 发现模型"}]}`
		if status.Load() != 200 {
			body = credential
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	server, code := fixtureWithBrowser(t, true, nil, service.CatalogOptions{DeepSeekBaseURL: upstream.URL})
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	if err := server.Bots.EnsureDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "快速接入测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	input := map[string]string{"preset": "deepseek", "api_key": credential}
	if response := request(t, handler, "POST", "/api/v1/providers/connect", input, nil); response.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatal("未认证请求访问供应商")
	}
	response := request(t, handler, "GET", "/api/v1/provider-presets", nil, headers)
	var presets []service.ProviderPreset
	if err = json.Unmarshal(response.Body.Bytes(), &presets); err != nil || response.Code != 200 || len(presets) != 1 || presets[0].ID != "deepseek" {
		t.Fatal("供应商预设契约错误", err)
	}
	response = request(t, handler, "POST", "/api/v1/providers/connect", input, headers)
	var result service.ProviderConnectResult
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || len(result.Models) != 1 || !result.Models[0].IsDefault || result.Provider.Preset != "deepseek" || strings.Contains(response.Body.String(), credential) {
		t.Fatal("快速接入响应错误或泄露密钥", response.Code, err)
	}
	response = request(t, handler, "GET", "/api/v1/providers", nil, headers)
	var providers []domain.Provider
	if err = json.Unmarshal(response.Body.Bytes(), &providers); err != nil || len(providers) != 1 || providers[0].Preset != "deepseek" || len(providers[0].ModelLimitDefaults) != 1 {
		t.Fatal("供应商列表缺少预设或默认限额说明", err)
	}
	status.Store(401)
	response = request(t, handler, "POST", "/api/v1/providers/connect", input, headers)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "PROVIDER_AUTH_FAILED") || strings.Contains(response.Body.String(), credential) {
		t.Fatal("上游认证失败误清登录或泄露原始响应", response.Code)
	}
	if response = request(t, handler, "GET", "/api/v1/providers", nil, headers); response.Code != 200 {
		t.Fatal("上游认证失败破坏本地会话")
	}
}
