package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/infra/bus"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestDesktopAPIWorksBeforeBootstrap(t *testing.T) {
	db := testkit.Database(t)
	directory := t.TempDir()
	authentication, err := auth.New(db, directory)
	if err != nil {
		t.Fatal(err)
	}
	code, err := authentication.IssueSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	events := bus.New(db)
	provisioner, err := provision.New(context.Background(), db, events, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := service.New(service.Dependencies{DB: db, Key: key, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Auth: authentication, Workspace: workspace, Provision: provisioner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now()}
	handler := server.Handler()

	blocked := request(t, handler, http.MethodGet, "/api/v1/bots", nil, nil)
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("引导未完成时原业务接口应继续拒绝: %d", blocked.Code)
	}
	session := request(t, handler, http.MethodGet, "/api/v1/auth/status", nil, nil)
	if session.Code != http.StatusOK || !json.Valid(session.Body.Bytes()) || !strings.Contains(session.Body.String(), `"initialized":false`) {
		t.Fatalf("桌面会话状态不正确: %d %s", session.Code, session.Body.String())
	}
	denied := request(t, handler, http.MethodGet, "/api/v1/workspace", nil, nil)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("未登录不应读取工作空间: %d", denied.Code)
	}
	password := testkit.Random(t) + "desktop"
	setup := request(t, handler, http.MethodPost, "/api/v1/auth/setup", map[string]string{"setupCode": code, "password": password}, nil)
	if setup.Code != http.StatusCreated {
		t.Fatalf("初始化失败: %d %s", setup.Code, setup.Body.String())
	}
	wrong := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]any{"password": "not-the-password", "deviceName": "TomBot", "platform": "linux"}, nil)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码应拒绝: %d", wrong.Code)
	}
	login := request(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]any{"password": password, "deviceName": "TomBot 桌面", "platform": "linux"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败: %d %s", login.Code, login.Body.String())
	}
	var sessionBody struct {
		Token string `json:"device_token"`
	}
	if err = json.Unmarshal(login.Body.Bytes(), &sessionBody); err != nil || sessionBody.Token == "" {
		t.Fatal("登录没有返回设备凭据", err)
	}
	loaded := request(t, handler, http.MethodGet, "/api/v1/workspace", nil, map[string]string{"Authorization": "Bearer " + sessionBody.Token})
	if loaded.Code != http.StatusOK || !json.Valid(loaded.Body.Bytes()) || !strings.Contains(loaded.Body.String(), `"agents":[]`) || strings.Contains(loaded.Body.String(), "小黑") {
		t.Fatalf("登录后应看到空工作空间: %d %s", loaded.Code, loaded.Body.String())
	}
}
