package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

type identityStub struct {
	authapi.Identity
	session identity.Session
}

func (stub *identityStub) Login(context.Context, identity.LoginRequest, identity.Attempt) (identity.Session, error) {
	return stub.session, nil
}
func (stub *identityStub) Authenticate(_ context.Context, value, scope string) (identity.Principal, error) {
	if scope != "admin" || value != string(stub.session.AccessToken) {
		return identity.Principal{}, identity.ErrInvalidSession
	}
	return stub.session.Principal, nil
}

type accountsStub struct {
	operation  accounts.Operation
	activation accounts.Activation
	err        error
	calls      int
	request    accounts.CreateRequest
	principal  identity.Principal
	requestID  string
	before     string
	limit      int
}

func (stub *accountsStub) ListUsers(_ context.Context, principal identity.Principal, before string, limit int) ([]accounts.UserSummary, error) {
	stub.calls++
	stub.principal, stub.before, stub.limit = principal, before, limit
	return []accounts.UserSummary{}, stub.err
}
func (stub *accountsStub) ListNodes(ctx context.Context, principal identity.Principal, before string, limit int) ([]accounts.NodeSummary, error) {
	_, err := stub.ListUsers(ctx, principal, before, limit)
	return []accounts.NodeSummary{}, err
}
func (stub *accountsStub) ListImages(ctx context.Context, principal identity.Principal, before string, limit int) ([]accounts.ImageSummary, error) {
	_, err := stub.ListUsers(ctx, principal, before, limit)
	return []accounts.ImageSummary{}, err
}

func (stub *accountsStub) Create(_ context.Context, principal identity.Principal, request accounts.CreateRequest) (accounts.Operation, error) {
	stub.calls++
	stub.request = request
	stub.principal = principal
	stub.requestID = request.RequestID
	return stub.operation, stub.err
}
func (stub *accountsStub) Get(_ context.Context, principal identity.Principal, _ string) (accounts.Operation, error) {
	stub.calls++
	stub.principal = principal
	return stub.operation, stub.err
}
func (stub *accountsStub) Retry(_ context.Context, principal identity.Principal, _, requestID string) (accounts.Operation, error) {
	stub.calls++
	stub.principal = principal
	stub.requestID = requestID
	return stub.operation, stub.err
}
func (stub *accountsStub) Reconcile(ctx context.Context, principal identity.Principal, operationID, requestID string) (accounts.Operation, error) {
	return stub.Retry(ctx, principal, operationID, requestID)
}
func (stub *accountsStub) IssueActivation(_ context.Context, principal identity.Principal, _, requestID string) (accounts.Activation, error) {
	stub.calls++
	stub.principal = principal
	stub.requestID = requestID
	return stub.activation, stub.err
}

type fixture struct {
	handler  *Handler
	accounts *accountsStub
	identity *identityStub
	cookies  []*http.Cookie
	csrf     string
	log      *bytes.Buffer
	models   *modelsStub
}

func randomValue(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	identityService := &identityStub{session: identity.Session{Principal: identity.Principal{UserID: ids.New(), SessionID: ids.New(), DeviceID: ids.New(), Role: "admin", Scope: "admin"},
		AccessToken: identity.Credential(randomValue(t)), RefreshToken: identity.Credential(randomValue(t)), AccessExpiresAt: time.Now().Add(time.Minute), ExpiresAt: time.Now().Add(time.Hour)}}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	log := new(bytes.Buffer)
	logger := slog.New(slog.NewTextHandler(log, nil))
	gate, err := authapi.New(identityService, authapi.Options{AdminOrigin: "https://admin.example.test", CSRFKey: key, RequestTimeout: time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "https://admin.example.test/api/v2/admin/auth/login", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://admin.example.test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("测试管理登录失败", response.Code)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	service := &accountsStub{operation: accounts.Operation{ID: ids.New(), UserID: ids.New(), ComputerID: ids.New(), Status: "succeeded", Stage: "completed", Attempt: 1},
		activation: accounts.Activation{UserID: ids.New(), Value: identity.Credential(randomValue(t)), ExpiresAt: time.Now().Add(time.Hour)}}
	models := &modelsStub{}
	handler, err := New(service, models, gate, logger)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{handler, service, identityService, response.Result().Cookies(), session.CSRF, log, models}
}

func (fixture fixture) request(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, "https://admin.example.test"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://admin.example.test")
	request.Header.Set("X-CSRF-Token", fixture.csrf)
	for _, cookie := range fixture.cookies {
		request.AddCookie(cookie)
	}
	return request
}

func (fixture fixture) run(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("管理响应状态错误：%d，预期 %d", response.Code, status)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Request-ID") == "" {
		t.Fatal("管理接口缺少缓存限制或请求标识")
	}
}

func TestCreationOperationProtocol(t *testing.T) {
	fixture := newFixture(t)
	response := fixture.run(fixture.request("POST", "/api/v2/admin/users", `{"username":"new-user","display_name":"新用户","role":"user","idempotency_key":"request-key"}`))
	requireStatus(t, response, 201)
	if fixture.accounts.request.RequestID == "" || fixture.accounts.principal.UserID != fixture.identity.session.Principal.UserID || fixture.accounts.request.IdempotencyKey != "request-key" {
		t.Fatal("未固定管理身份或透传幂等键")
	}
	if response.Header().Get("Location") != "/api/v2/admin/user-creation-operations/"+fixture.accounts.operation.ID {
		t.Fatal("未返回操作查询地址")
	}
	for _, test := range []struct {
		err             error
		status          int
		operationStatus string
	}{
		{accounts.ErrPending, 504, "running"}, {accounts.ErrProvisionFailed, 502, "failed"}, {accounts.ErrQuarantined, 409, "quarantined"},
		{accounts.ErrConflict, 409, ""}, {accounts.ErrNoCapacity, 503, ""}, {identity.ErrForbidden, 403, ""},
		{nil, 503, "pending"},
	} {
		fixture.accounts.err = test.err
		fixture.accounts.operation.Status = test.operationStatus
		response = fixture.run(fixture.request("POST", "/api/v2/admin/users", `{}`))
		requireStatus(t, response, test.status)
		if errors.Is(test.err, accounts.ErrPending) && !strings.Contains(response.Body.String(), fixture.accounts.operation.ID) {
			t.Fatal("超时响应丢失操作标识")
		}
	}
	fixture.accounts.err = nil
	fixture.accounts.operation.Status = "succeeded"
	response = fixture.run(fixture.request("GET", "/api/v2/admin/user-creation-operations/"+fixture.accounts.operation.ID, ""))
	requireStatus(t, response, 200)
	response = fixture.run(fixture.request("POST", "/api/v2/admin/user-creation-operations/"+fixture.accounts.operation.ID+"/retry", `{}`))
	requireStatus(t, response, 200)
	fixture.accounts.operation.Status = "failed"
	response = fixture.run(fixture.request("POST", "/api/v2/admin/user-creation-operations/"+fixture.accounts.operation.ID+"/reconcile", `{}`))
	requireStatus(t, response, 200)
	response = fixture.run(fixture.request("POST", "/api/v2/admin/users/"+fixture.accounts.operation.UserID+"/activation", `{}`))
	requireStatus(t, response, 201)
	if !strings.Contains(response.Body.String(), string(fixture.accounts.activation.Value)) {
		t.Fatal("专用签发响应缺失激活材料")
	}
	if strings.Contains(fixture.log.String(), string(fixture.accounts.activation.Value)) {
		t.Fatal("激活材料进入日志")
	}
}

func TestManagementBoundariesAndFailureRedaction(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		body   string
		status int
	}{
		{"无会话", func(r *http.Request) { r.Header.Del("Cookie") }, `{}`, 403},
		{"无 CSRF", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, `{}`, 403},
		{"其他来源", func(r *http.Request) { r.Header.Set("Origin", "https://other.example.test") }, `{}`, 403},
		{"桌面凭据", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+randomValue(t)) }, `{}`, 400},
		{"伪造操作者", func(*http.Request) {}, `{"actor_id":"other"}`, 400},
		{"伪造请求标识", func(*http.Request) {}, `{"request_id":"other"}`, 400},
		{"未知字段", func(*http.Request) {}, `{"unexpected":true}`, 400},
		{"查询凭据", func(r *http.Request) { r.URL.RawQuery = "access_token=ignored" }, `{}`, 400},
		{"超长请求", func(*http.Request) {}, `{"username":"` + strings.Repeat("a", 16<<10) + `"}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			request := fixture.request("POST", "/api/v2/admin/users", test.body)
			test.change(request)
			requireStatus(t, fixture.run(request), test.status)
			if fixture.accounts.calls != 0 {
				t.Fatal("无效请求到达开户服务")
			}
		})
	}
	fixture := newFixture(t)
	marker := randomValue(t)
	fixture.accounts.err = errors.New(marker)
	response := fixture.run(fixture.request("POST", "/api/v2/admin/users", `{}`))
	requireStatus(t, response, 503)
	if strings.Contains(response.Body.String(), marker) || strings.Contains(fixture.log.String(), marker) {
		t.Fatal("原始服务异常泄露")
	}
	for _, path := range []string{"/api/v2/admin/bootstrap", "/api/v2/admin/users/initial", "/api/v2/admin/user-creation-operations/initial/bootstrap-activation"} {
		requireStatus(t, fixture.run(fixture.request("POST", path, `{}`)), 404)
	}
}
