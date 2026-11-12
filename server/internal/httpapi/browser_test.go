package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestBrowserHTTPAuthenticationControlAndContracts(t *testing.T) {
	t.Parallel()
	server, code := fixture(t, true)
	handler := server.Handler()
	if response := request(t, handler, "GET", "/api/v1/browser/tabs", nil, nil); response.Code != 401 {
		t.Fatal("浏览器缺少认证门禁", response.Code)
	}
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "浏览器测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	response := request(t, handler, "POST", "/api/v1/browser/tabs", map[string]string{"url": "https://example.com"}, headers)
	if response.Code != 201 {
		t.Fatal("无法打开标签", response.Code, response.Body.String())
	}
	var tab service.BrowserTab
	if err = json.Unmarshal(response.Body.Bytes(), &tab); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/browser/tabs/" + tab.ID
	response = request(t, handler, "GET", base+"/snapshot", nil, headers)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "e1") {
		t.Fatal("快照契约错误", response.Code)
	}
	response = request(t, handler, "GET", base+"/screenshot", nil, headers)
	if response.Code != 200 || response.Header().Get("Content-Type") != "image/jpeg" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Browser-Origin") != "https://example.com" {
		t.Fatal("截图契约错误", response.Code)
	}
	input := map[string]any{"action": "key", "key": "Enter", "revision": tab.Revision, "page_revision": tab.PageRevision}
	response = request(t, handler, "POST", base+"/actions", input, headers)
	if response.Code != 200 {
		t.Fatal("动作失败", response.Code, response.Body.String())
	}
	response = request(t, handler, "POST", base+"/actions", input, headers)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "BROWSER_TAB_CHANGED") {
		t.Fatal("旧页面动作未拒绝", response.Code)
	}
	response = request(t, handler, "POST", base+"/control", map[string]any{"controller": "bot", "revision": tab.Revision}, headers)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "BROWSER_NO_BOT") {
		t.Fatal("共享标签可交给未关联 Bot")
	}
	for _, input := range []any{map[string]any{"action": "evaluate", "script": "document.title"}, map[string]any{"action": "key", "key": "Enter", "url": "https://example.com"}, map[string]any{"action": "click", "ref": "e1"}} {
		response = request(t, handler, "POST", base+"/actions", input, headers)
		if response.Code != 400 && response.Code != 409 {
			t.Fatal("无效动作被接受", response.Code)
		}
	}
	response = request(t, handler, "DELETE", base, nil, headers)
	if response.Code != 400 {
		t.Fatal("关闭缺少版本校验")
	}
	response = request(t, handler, "DELETE", base+"?revision="+strconv.FormatUint(tab.Revision, 10), nil, headers)
	if response.Code != http.StatusNoContent {
		t.Fatal("关闭失败", response.Code)
	}
	response = request(t, handler, "GET", base, nil, headers)
	if response.Code != 404 {
		t.Fatal("关闭后仍返回标签")
	}
	if err = server.Auth.Revoke(context.Background(), session.DeviceID, session.DeviceID); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "GET", "/api/v1/browser/status", nil, headers)
	if response.Code != 401 {
		t.Fatal("撤销设备仍可访问浏览器")
	}
}

func TestBrowserHTTPReadinessGate(t *testing.T) {
	t.Parallel()
	server, _ := fixture(t, false)
	response := request(t, server.Handler(), "GET", "/api/v1/browser/tabs", nil, nil)
	if response.Code != 503 || !strings.Contains(response.Body.String(), "BOOTSTRAP_NOT_READY") {
		t.Fatal("引导未完成仍可访问浏览器")
	}
}
