package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/provision"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/testkit/browserfake"
	"io.yueyang/tom/internal/tools"
	"io.yueyang/tom/internal/workspace"
	"os"
	"path/filepath"
)

func fixture(t *testing.T, ready bool) (*Server, string) {
	t.Helper()
	return fixtureWithBrowser(t, ready, nil)
}

func TestAuthStatusExposesServerVersionBeforeLogin(t *testing.T) {
	server, _ := fixture(t, true)
	server.Version = "1.2.3-dev"
	response := request(t, server.Handler(), "GET", "/api/v1/auth/status", nil, nil)
	var status struct {
		Version       string `json:"server_version"`
		Authenticated bool   `json:"authenticated"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || status.Version != server.Version || status.Authenticated {
		t.Fatal("登录前未返回实例的实际版本或认证状态错误")
	}
}

func fixtureWithBrowser(t *testing.T, ready bool, browserFactory func(context.Context, browser.Options) (service.BrowserEngine, error), catalogOptions ...service.CatalogOptions) (*Server, string) {
	t.Helper()
	db := testkit.Database(t)
	dir := t.TempDir()
	authentication, err := auth.New(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := authentication.IssueSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	events := bus.New(db)
	p, err := provision.New(context.Background(), db, events, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		if !p.Claim() {
			t.Fatal("无法领取引导")
		}
		if err = p.RunClaimed(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	workspaceDir := t.TempDir()
	if err = os.MkdirAll(filepath.Join(workspaceDir, "shared"), 0700); err != nil {
		t.Fatal(err)
	}
	storage, err := workspace.Open(workspaceDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	server := &Server{Audit: service.NewAuditLog(db), Groups: service.NewGroups(db, events), Approvals: service.NewApprovals(db, events), Catalog: service.NewCatalog(db, catalogOptions...), Bots: service.NewBots(db, events, workspaceDir), Conversations: service.NewConversations(db, events), Secrets: secret.New(db, make([]byte, 32)), Config: config.Config{DataDir: dir, WorkspaceDir: workspaceDir}, Auth: authentication, Provision: p, Bus: events, Key: make([]byte, 32), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Started: time.Now(), RootContext: context.Background()}
	server.Version = "0.1.0-test"
	server.Memories = service.NewMemories(db, server.Bots, storage, events)
	server.Files = service.NewFiles(db, server.Bots, storage, events, server.Memories)
	if browserFactory == nil {
		browserFactory = func(ctx context.Context, options browser.Options) (service.BrowserEngine, error) {
			return browserfake.New(options), nil
		}
	}
	server.Browser, err = service.NewBrowser(context.Background(), service.BrowserDependencies{DB: db, Bots: server.Bots, Events: events, Logger: server.Logger, Factory: browserFactory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Browser.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	server.Terminals, err = service.NewTerminals(context.Background(), db, events, server.Bots, storage, server.Logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Terminals.Shutdown(shutdown); err != nil {
			t.Error(err)
		}
	})
	server.Settings = service.NewSettings(db)
	server.Usage = service.NewUsage(db)
	factory := func(spec domain.Provider, key []byte) provider.Provider { return provider.NewHTTP(spec, key) }
	reviewer, err := policy.NewReviewer(policy.ReviewerDependencies{DB: db, Settings: server.Settings, Catalog: server.Catalog, Secrets: server.Secrets, Factory: factory, Logger: server.Logger})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := tools.New(db, events, storage, server.Approvals, server.Secrets, server.Terminals, server.Browser, reviewer, server.Memories)
	if err != nil {
		t.Fatal(err)
	}
	server.Runtime, err = agent.New(context.Background(), agent.Dependencies{Files: storage, Tools: executor, DB: db, Events: events, Catalog: server.Catalog, Bots: server.Bots, Memories: server.Memories, Secrets: server.Secrets, Logger: server.Logger, Factory: factory}, agent.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Runtime.Close)
	server.Routines = service.NewRoutines(db, events, server.Runtime)
	if err = server.Routines.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return server, code
}
func request(t *testing.T, handler http.Handler, method, path string, payload any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestReadinessGateAndSetupContract(t *testing.T) {
	server, _ := fixture(t, false)
	handler := server.Handler()
	for _, path := range []string{"/api/v1/auth/status", "/api/v1/devices", "/api/v1/bots"} {
		response := request(t, handler, "GET", path, nil, nil)
		if response.Code != 503 || !strings.Contains(response.Body.String(), "BOOTSTRAP_NOT_READY") {
			t.Fatalf("就绪门禁失效: %s %d", path, response.Code)
		}
	}
	server, code := fixture(t, true)
	handler = server.Handler()
	password := testkit.Random(t)
	headers := map[string]string{"X-Tom-Setup-Code": code}
	for _, extra := range []string{"provider", "model", "api_key"} {
		response := request(t, handler, "POST", "/api/v1/auth/setup", map[string]string{"password": password, extra: "unsupported"}, headers)
		if response.Code != 400 || server.Auth.Initialized() {
			t.Fatalf("额外初始化字段被接受: %s", extra)
		}
	}
	response := request(t, handler, "POST", "/api/v1/auth/setup", map[string]string{"password": password}, headers)
	if response.Code != 201 {
		t.Fatalf("初始化失败: %d %s", response.Code, response.Body.String())
	}
	response = request(t, handler, "POST", "/api/v1/auth/setup", map[string]string{"password": password}, headers)
	if response.Code != 409 {
		t.Fatal("重复初始化没有返回冲突")
	}
}
func TestDeviceAuthenticationRevocationAndErrors(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	response := request(t, handler, "POST", "/api/v1/auth/login", map[string]any{"password": password, "device": map[string]string{"name": "测试桌面", "platform": "macos"}}, nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var session auth.Session
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	response = request(t, handler, "GET", "/api/v1/devices", nil, headers)
	if response.Code != 200 || strings.Contains(response.Body.String(), session.DeviceToken) {
		t.Fatal("设备接口泄漏或失败")
	}
	response = request(t, handler, "DELETE", "/api/v1/devices/"+session.DeviceID, nil, headers)
	if response.Code != 204 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "GET", "/api/v1/devices", nil, headers)
	if response.Code != 401 || response.Header().Get("X-Request-ID") == "" || !strings.Contains(response.Body.String(), "request_id") {
		t.Fatal("撤销或错误契约失败")
	}
}
func TestHealthIsReadOnlyAndDoesNotLeakBusiness(t *testing.T) {
	server, _ := fixture(t, true)
	response := request(t, server.Handler(), "GET", "/health", nil, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "tom") || strings.Contains(response.Body.String(), server.Config.DataDir) || strings.Contains(response.Body.String(), "<script") {
		t.Fatal("健康页泄漏或出现脚本")
	}
	server.Config.HealthToken = testkit.Random(t)
	response = request(t, server.Handler(), "GET", "/health", nil, nil)
	if response.Code != 401 {
		t.Fatal("健康页口令未生效")
	}
	response = request(t, server.Handler(), "GET", "/health?t="+server.Config.HealthToken, nil, nil)
	if response.Code != 200 || strings.Contains(response.Body.String(), server.Config.HealthToken) {
		t.Fatal("健康页鉴权失败或泄漏")
	}
}
func TestSSEBootstrapReplay(t *testing.T) {
	server, _ := fixture(t, true)
	id, err := server.Bus.Publish(context.Background(), "bootstrap.step", "", map[string]string{"phase": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server.RootContext = ctx
	handler := server.Handler()
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(httpServer.URL + "/api/v1/bootstrap/events?after=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	chunk := make([]byte, 2048)
	var body strings.Builder
	for !strings.Contains(body.String(), "event: bootstrap.step") {
		n, err := response.Body.Read(chunk)
		body.Write(chunk[:n])
		if err != nil {
			t.Fatal(err)
		}
	}
	if id <= 1 || !strings.Contains(body.String(), "ready") {
		t.Fatal("SSE 没有续传状态")
	}
	cancel()
}
