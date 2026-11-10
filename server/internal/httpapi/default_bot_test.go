package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestDefaultBotSetupFailureCanRecoverOnLoginWithoutResettingAuthentication(t *testing.T) {
	server, code := fixture(t, true)
	handler := server.Handler()
	password := testkit.Random(t)
	login := map[string]any{"password": password, "deviceName": "默认助手验收", "platform": "macos"}
	if response := request(t, handler, "POST", "/api/v1/auth/login", login, nil); response.Code != http.StatusConflict {
		t.Fatal("未初始化时不应登录或创建助手", response.Code)
	}
	if bots, err := server.Bots.List(context.Background(), "", 10, false); err != nil || len(bots) != 0 {
		t.Fatal("认证提交前创建了业务数据", bots, err)
	}
	conflict := filepath.Join(server.Config.WorkspaceDir, "bots")
	if err := os.WriteFile(conflict, []byte("目录冲突"), 0600); err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, "POST", "/api/v1/auth/setup", map[string]string{"password": password, "setupCode": code}, nil)
	if response.Code != http.StatusInternalServerError || !server.Auth.Initialized() {
		t.Fatal("目录失败未显式报告或错误回滚了已提交认证", response.Code)
	}
	if _, err := server.Bots.Get(context.Background(), service.DefaultBotID); err == nil {
		t.Fatal("目录失败留下了半成品助手")
	}
	if response = request(t, handler, "POST", "/api/v1/auth/login", login, nil); response.Code != http.StatusInternalServerError {
		t.Fatal("目录仍失败时没有阻止登录", response.Code)
	}
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "POST", "/api/v1/auth/login", login, nil)
	if response.Code != http.StatusOK {
		t.Fatal("认证已提交后无法通过正常登录恢复助手准备", response.Code, response.Body.String())
	}
	var session auth.Session
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	bot, err := server.Bots.Get(context.Background(), service.DefaultBotID)
	if err != nil || bot.Name != "Tom" || bot.ConversationID != "tom-direct" || bot.ModelID != nil {
		t.Fatal("登录后默认助手身份不完整", bot, err)
	}
	for _, part := range []string{"inbox", "out", "notes", ".tom/memory"} {
		entry, err := os.Stat(filepath.Join(server.Config.WorkspaceDir, bot.WorkspaceDir, part))
		if err != nil || !entry.IsDir() {
			t.Fatal("登录前没有准备真实目录", part, err)
		}
	}
	response = request(t, handler, "GET", "/api/v1/bots/tom", nil, headers)
	if response.Code != http.StatusOK {
		t.Fatal("默认助手无法通过普通 API 读取", response.Code)
	}
	response = request(t, handler, "DELETE", "/api/v1/bots/tom?confirm_name=Tom&purge_workspace=true", nil, headers)
	if response.Code != http.StatusNoContent {
		t.Fatal("默认助手不可正常删除", response.Code, response.Body.String())
	}
	if response = request(t, handler, "POST", "/api/v1/auth/login", login, nil); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err = server.Bots.Get(context.Background(), service.DefaultBotID); err == nil {
		t.Fatal("登录重新创建了已删除的助手")
	}
}
