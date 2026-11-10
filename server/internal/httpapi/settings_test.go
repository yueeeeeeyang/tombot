package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/testkit"
)

func TestSettingsHTTPAuthenticationPatchAndValidation(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "审查配置测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	if response := request(t, handler, "GET", "/api/v1/settings", nil, nil); response.Code != 401 {
		t.Fatal("审查设置未受鉴权保护", response.Code)
	}
	response := request(t, handler, "GET", "/api/v1/settings", nil, headers)
	var settings domain.Settings
	if err = json.Unmarshal(response.Body.Bytes(), &settings); err != nil || response.Code != 200 || settings.Reviewer.Enabled {
		t.Fatal("初始设置响应错误", response.Code, err)
	}
	response = request(t, handler, "PATCH", "/api/v1/settings", map[string]any{"reviewer": map[string]any{"enabled": true}}, headers)
	if response.Code != 200 {
		t.Fatal("设置未保存", response.Code)
	}
	response = request(t, handler, "GET", "/api/v1/settings", nil, headers)
	if err = json.Unmarshal(response.Body.Bytes(), &settings); err != nil || !settings.Reviewer.Enabled || settings.Reviewer.ModelID != nil {
		t.Fatal("设置未持久化", err)
	}
	for _, input := range []any{nil, map[string]any{}, map[string]any{"reviewer": nil}, map[string]any{"reviewer": map[string]any{"enabled": nil}}, map[string]any{"reviewer": map[string]any{"enabled": "true"}}, map[string]any{"reviewer": map[string]any{"unknown": true}}, map[string]any{"unknown": true}} {
		if response = request(t, handler, "PATCH", "/api/v1/settings", input, headers); response.Code != 400 {
			t.Fatal("无效设置请求被接受", input, response.Code)
		}
	}
}
