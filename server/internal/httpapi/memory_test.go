package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestMemoryHTTPCRUDSearchAndConditionalWrites(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := server.Auth.Login(ctx, password, "记忆管理测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	base := "/api/v1/bots/" + bot.ID + "/memory"
	if response := request(t, handler, "GET", base, nil, nil); response.Code != 401 {
		t.Fatal("记忆缺少鉴权")
	}
	input := domain.MemoryInput{Title: "资料核验", Content: "优先官方文档并交叉核验。", Tags: []string{"research"}, Importance: 4}
	response := request(t, handler, "POST", base, input, headers)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var entry domain.Memory
	if err = json.Unmarshal(response.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/memory/" + bot.ID + "?" + url.Values{"path": {entry.Path}}.Encode()
	for _, endpoint := range []string{base, base + "?query=" + url.QueryEscape("核验"), path} {
		response = request(t, handler, "GET", endpoint, nil, headers)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "资料核验") {
			t.Fatal(endpoint, response.Code, response.Body.String())
		}
	}
	input.Content = "改用官方一手资料。"
	for _, invalidPath := range []string{"", "?path=shared/test.md"} {
		response = request(t, handler, "PATCH", "/api/v1/memory/"+bot.ID+invalidPath, memoryEdit{MemoryInput: input, ExpectedSHA256: entry.SHA256}, headers)
		if response.Code != 400 {
			t.Fatal("编辑接受了缺失或非私有路径", response.Code)
		}
	}
	response = request(t, handler, "PATCH", path, memoryEdit{MemoryInput: input, ExpectedSHA256: entry.SHA256}, headers)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var updated domain.Memory
	if err = json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "PATCH", path, memoryEdit{MemoryInput: input, ExpectedSHA256: entry.SHA256}, headers)
	if response.Code != 409 {
		t.Fatal("过时编辑未被拒绝", response.Code)
	}
	response = request(t, handler, "GET", "/api/v1/memory/"+bot.ID+"?path=shared/test.md", nil, headers)
	if response.Code != 400 {
		t.Fatal("非记忆路径被接受", response.Code)
	}
	response = request(t, handler, "POST", base+"/reindex", nil, headers)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, handler, "DELETE", path+"&expected_sha256="+updated.SHA256, nil, headers)
	if response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, handler, "GET", base, nil, headers)
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatal("删除后列表仍有记录", response.Code, response.Body.String())
	}
}
