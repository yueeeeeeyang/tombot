package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestSecretsHTTPMetadataOnlyRotationAndReferencedDelete(t *testing.T) {
	s, code := fixture(t, true)
	ctx := context.Background()
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(ctx, password, "密钥管理测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := s.Handler()
	if response := request(t, handler, "GET", "/api/v1/secrets", nil, nil); response.Code != 401 {
		t.Fatal(response.Code)
	}
	value := testkit.Random(t)
	replacement := testkit.Random(t)
	response := request(t, handler, "POST", "/api/v1/secrets", map[string]string{"name": "项目调用", "value": value}, headers)
	var metadata secret.Metadata
	if err = json.Unmarshal(response.Body.Bytes(), &metadata); err != nil || response.Code != 201 || metadata.ID == "" || strings.Contains(response.Body.String(), value) {
		t.Fatal("创建响应或脱敏错误", response.Code, err)
	}
	response = request(t, handler, "POST", "/api/v1/secrets/"+metadata.ID+"/rotate", map[string]string{"value": replacement}, headers)
	if response.Code != 200 || strings.Contains(response.Body.String(), replacement) {
		t.Fatal("轮换失败或泄漏", response.Code)
	}
	response = request(t, handler, "GET", "/api/v1/secrets?limit=1", nil, headers)
	var records []secret.Metadata
	if err = json.Unmarshal(response.Body.Bytes(), &records); err != nil || len(records) != 1 || records[0].RotatedAt == nil || strings.Contains(response.Body.String(), value) || strings.Contains(response.Body.String(), replacement) {
		t.Fatal("列表未脱敏或未更新", err)
	}
	response = request(t, handler, "GET", "/api/v1/secrets?limit=1&cursor="+metadata.ID, nil, headers)
	if err = json.Unmarshal(response.Body.Bytes(), &records); err != nil || len(records) != 0 {
		t.Fatal("密钥分页错误", err)
	}
	for _, input := range []map[string]string{{"name": "", "value": value}, {"name": "过短", "value": value[:7]}, {"name": "过长", "value": strings.Repeat(value, 1000)}} {
		if response = request(t, handler, "POST", "/api/v1/secrets", input, headers); response.Code != 400 {
			t.Fatal("无效密钥被接受", response.Code)
		}
	}
	provider, err := s.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "测试模型服务", BaseURL: "http://127.0.0.1:1234", SecretID: &metadata.ID}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if response = request(t, handler, "DELETE", "/api/v1/secrets/"+metadata.ID, nil, headers); response.Code != 409 {
		t.Fatal("引用中仍可删除", response.Code)
	}
	if err = s.Catalog.DeleteProvider(ctx, provider.ID, "user"); err != nil {
		t.Fatal(err)
	}
	if response = request(t, handler, "DELETE", "/api/v1/secrets/"+metadata.ID, nil, headers); response.Code != 204 {
		t.Fatal(response.Code)
	}
	if response = request(t, handler, "POST", "/api/v1/secrets/"+metadata.ID+"/rotate", map[string]string{"value": replacement}, headers); response.Code != 404 {
		t.Fatal(response.Code)
	}
}
