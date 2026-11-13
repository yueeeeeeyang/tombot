package authapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// identityStub 只替换传输边界；身份语义另由真实 PostgreSQL 集成测试覆盖。
type identityStub struct {
	session            identity.Session
	err                error
	calls              []string
	loginRequest       identity.LoginRequest
	attempt            identity.Attempt
	principal          identity.Principal
	refresh            string
	scope              string
	hasDeadline        bool
	authorizationID    string
	authorizationState string
	authorizationCode  string
	redirectURI        string
	challenge          string
}

func (stub *identityStub) record(name string, ctx context.Context, attempt identity.Attempt) {
	stub.calls = append(stub.calls, name)
	stub.attempt = attempt
	_, stub.hasDeadline = ctx.Deadline()
}
func (stub *identityStub) Activate(ctx context.Context, _, _, _ string, attempt identity.Attempt) error {
	stub.record("activate", ctx, attempt)
	return stub.err
}
func (stub *identityStub) AdministratorsExist(ctx context.Context) (bool, error) {
	stub.calls = append(stub.calls, "administrators-exist")
	return false, stub.err
}
func (stub *identityStub) CreateFirstAdministrator(ctx context.Context, username, displayName, _ string, attempt identity.Attempt) (identity.AdministratorSetup, error) {
	stub.record("create-first-administrator", ctx, attempt)
	return identity.AdministratorSetup{AdministratorID: ids.New(), Username: username, DisplayName: displayName, Secret: identity.Credential("测试验证器密钥"), Period: 30, Digits: 6, Algorithm: "SHA1"}, stub.err
}
func (stub *identityStub) Login(ctx context.Context, request identity.LoginRequest, attempt identity.Attempt) (identity.Session, error) {
	stub.record("login", ctx, attempt)
	stub.loginRequest = request
	return stub.session, stub.err
}
func (stub *identityStub) Refresh(ctx context.Context, token, scope string, attempt identity.Attempt) (identity.Session, error) {
	stub.record("refresh", ctx, attempt)
	stub.refresh = token
	stub.scope = scope
	return stub.session, stub.err
}
func (stub *identityStub) Authenticate(ctx context.Context, token, scope string) (identity.Principal, error) {
	stub.record("authenticate", ctx, identity.Attempt{})
	stub.scope = scope
	if token != string(stub.session.AccessToken) {
		return identity.Principal{}, identity.ErrInvalidSession
	}
	principal := stub.session.Principal
	principal.Scope = scope
	return principal, stub.err
}
func (stub *identityStub) Logout(ctx context.Context, principal identity.Principal, requestID string) error {
	stub.record("logout", ctx, identity.Attempt{RequestID: requestID})
	stub.principal = principal
	return stub.err
}
func (stub *identityStub) ChangePassword(ctx context.Context, principal identity.Principal, _, _, _ string, attempt identity.Attempt) error {
	stub.record("password", ctx, attempt)
	stub.principal = principal
	return stub.err
}
func (stub *identityStub) Reauthenticate(ctx context.Context, principal identity.Principal, _, _ string, attempt identity.Attempt) error {
	stub.record("verify", ctx, attempt)
	stub.principal = principal
	return stub.err
}
func (stub *identityStub) ListDevices(ctx context.Context, principal identity.Principal, _ string, _ int) ([]identity.Device, error) {
	stub.record("devices", ctx, identity.Attempt{})
	stub.principal = principal
	return []identity.Device{}, stub.err
}
func (stub *identityStub) RevokeDevice(ctx context.Context, principal identity.Principal, _, requestID string) error {
	stub.record("revoke", ctx, identity.Attempt{RequestID: requestID})
	stub.principal = principal
	return stub.err
}

// 浏览器登录的传输测试只需要固定返回值；领域语义由身份服务集成测试覆盖。
func (stub *identityStub) BeginBrowserAuthorization(ctx context.Context, request identity.BrowserAuthorizationRequest) (identity.Authorization, error) {
	stub.record("browser-start", ctx, identity.Attempt{})
	stub.redirectURI = request.RedirectURI
	return identity.Authorization{ID: stub.authorizationID, State: request.State, RedirectURI: request.RedirectURI, DeviceName: request.DeviceName, Platform: request.Platform}, stub.err
}
func (stub *identityStub) PendingAuthorization(ctx context.Context, _, _ string) (identity.Authorization, error) {
	stub.record("browser-pending", ctx, identity.Attempt{})
	return identity.Authorization{ID: stub.authorizationID, State: stub.authorizationState, RedirectURI: stub.redirectURI, DeviceName: "测试桌面", Platform: "linux"}, stub.err
}
func (stub *identityStub) ApproveBrowserAuthorization(ctx context.Context, _, _, _, _ string, attempt identity.Attempt) (string, error) {
	stub.record("browser-approve", ctx, attempt)
	return stub.authorizationCode, stub.err
}
func (stub *identityStub) ExchangeBrowserAuthorization(ctx context.Context, code, verifier, state string, attempt identity.Attempt) (identity.Session, error) {
	stub.record("browser-exchange", ctx, attempt)
	stub.authorizationCode, stub.challenge, stub.authorizationState = code, verifier, state
	return stub.session, stub.err
}

func randomValue(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal("生成测试随机值失败")
	}
	return value
}

func fixture(t *testing.T) (*Handler, *identityStub, *bytes.Buffer) {
	t.Helper()
	stub := &identityStub{session: identity.Session{Principal: identity.Principal{UserID: ids.New(), SessionID: ids.New(), DeviceID: ids.New(), Role: "admin", Scope: "admin"},
		AccessToken: identity.Credential(randomValue(t)), RefreshToken: identity.Credential(randomValue(t)), AccessExpiresAt: time.Now().Add(5 * time.Minute), ExpiresAt: time.Now().Add(time.Hour)}}
	key, err := secure.Random(32)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	handler, err := New(stub, Options{AdminOrigin: "https://admin.example.test", CSRFKey: key, RequestTimeout: 10 * time.Second}, slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler, stub, &log
}

func request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "https://admin.example.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func adminRequest(handler *Handler, stub *identityStub, method, path, body string) *http.Request {
	r := request(method, "/api/v2/admin/auth"+path, body)
	r.Header.Set("Origin", handler.options.AdminOrigin)
	r.Header.Set("X-CSRF-Token", handler.csrf(string(stub.session.RefreshToken)))
	r.AddCookie(&http.Cookie{Name: accessCookie, Value: string(stub.session.AccessToken)})
	r.AddCookie(&http.Cookie{Name: refreshCookie, Value: string(stub.session.RefreshToken)})
	return r
}

func run(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, r)
	return result
}
func requireStatus(t *testing.T, result *httptest.ResponseRecorder, status int) {
	t.Helper()
	if result.Code != status {
		t.Fatalf("HTTP 状态错误：%d，预期 %d", result.Code, status)
	}
	if result.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("认证响应可被缓存")
	}
}

func TestLoginTransportBoundaries(t *testing.T) {
	handler, stub, _ := fixture(t)
	result := run(handler, request("POST", "/api/v2/auth/login", `{"username":"test-user","device_name":"桌面","platform":"linux"}`))
	requireStatus(t, result, 200)
	if len(result.Header().Values("Set-Cookie")) != 0 || stub.loginRequest.Scope != "desktop" || !stub.hasDeadline {
		t.Fatal("桌面协议混入 Cookie 或缺少时限")
	}
	var desktop identity.Session
	if err := json.Unmarshal(result.Body.Bytes(), &desktop); err != nil {
		t.Fatal("桌面响应格式错误")
	}
	if desktop.AccessToken != stub.session.AccessToken || desktop.RefreshToken != stub.session.RefreshToken {
		t.Fatal("桌面未收到独立凭据响应")
	}
	result = run(handler, adminRequest(handler, stub, "POST", "/login", `{"username":"test-user","device_name":"管理平台"}`))
	requireStatus(t, result, 200)
	if stub.loginRequest.Scope != "admin" || stub.loginRequest.Platform != "web" {
		t.Fatal("管理会话范围不固定")
	}
	if bytes.Contains(result.Body.Bytes(), []byte(stub.session.AccessToken)) || bytes.Contains(result.Body.Bytes(), []byte(stub.session.RefreshToken)) {
		t.Fatal("管理响应向脚本暴露会话凭据")
	}
	cookies := result.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatal("管理会话缺少 Cookie")
	}
	for _, cookie := range cookies {
		if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || !strings.HasPrefix(cookie.Name, "__Host-") {
			t.Fatal("Cookie 安全属性不完整")
		}
	}
	var admin struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &admin); err != nil || admin.CSRFToken != handler.csrf(string(stub.session.RefreshToken)) {
		t.Fatal("管理响应没有绑定会话的 CSRF 校验值")
	}
}

func TestHTTPSOriginAndTrustedProxy(t *testing.T) {
	for _, scenario := range []string{"明文伪造代理", "缺少来源", "外部来源", "同站其他来源", "重复来源"} {
		t.Run(scenario, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			r := adminRequest(handler, stub, "POST", "/login", `{}`)
			status := 403
			switch scenario {
			case "明文伪造代理":
				r.TLS = nil
				r.Header.Set("X-Forwarded-Proto", "https")
				status = 400
			case "缺少来源":
				r.Header.Del("Origin")
			case "外部来源":
				r.Header.Set("Origin", "https://outside.example.test")
			case "同站其他来源":
				r.Header.Set("Sec-Fetch-Site", "same-site")
			case "重复来源":
				r.Header.Add("Origin", handler.options.AdminOrigin)
			}
			requireStatus(t, run(handler, r), status)
			if len(stub.calls) != 0 {
				t.Fatal("来源校验之前调用了身份服务")
			}
		})
	}
	handler, stub, _ := fixture(t)
	handler.options.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	r := request("POST", "/api/v2/auth/login", `{}`)
	r.TLS = nil
	r.RemoteAddr = "192.0.2.5:8080"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "198.51.100.8")
	requireStatus(t, run(handler, r), 200)
	if stub.attempt.Source != netip.MustParseAddr("198.51.100.8") {
		t.Fatal("未使用受信代理覆盖的来源")
	}
	stub.calls = nil
	r = request("POST", "/api/v2/auth/login", `{}`)
	r.TLS = nil
	r.RemoteAddr = "192.0.2.5:8080"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-For", "198.51.100.8, 203.0.113.9")
	requireStatus(t, run(handler, r), 400)
	if len(stub.calls) != 0 {
		t.Fatal("接受了未约定的代理链")
	}
}

func TestCSRFPrecedesMutationsAndBindsRefresh(t *testing.T) {
	for _, scenario := range []string{"缺少校验", "其他会话", "重复头", "缺少续期 Cookie", "重复 Cookie"} {
		t.Run(scenario, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			r := adminRequest(handler, stub, "POST", "/refresh", `{}`)
			switch scenario {
			case "缺少校验":
				r.Header.Del("X-CSRF-Token")
			case "其他会话":
				r.Header.Set("X-CSRF-Token", handler.csrf(randomValue(t)))
			case "重复头":
				r.Header.Add("X-CSRF-Token", r.Header.Get("X-CSRF-Token"))
			case "缺少续期 Cookie":
				r.Header.Del("Cookie")
			case "重复 Cookie":
				r.AddCookie(&http.Cookie{Name: refreshCookie, Value: string(stub.session.RefreshToken)})
			}
			requireStatus(t, run(handler, r), 403)
			if len(stub.calls) != 0 {
				t.Fatal("CSRF 拒绝前已调用身份服务")
			}
		})
	}
	handler, stub, _ := fixture(t)
	r := adminRequest(handler, stub, "POST", "/refresh", `{}`)
	previous := string(stub.session.RefreshToken)
	stub.session.RefreshToken = identity.Credential(randomValue(t))
	result := run(handler, r)
	requireStatus(t, result, 200)
	if stub.refresh != previous || stub.scope != "admin" {
		t.Fatal("续期未使用受保护的 Cookie")
	}
	var response struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil || response.CSRFToken != handler.csrf(string(stub.session.RefreshToken)) {
		t.Fatal("续期后 CSRF 未跟随轮换")
	}
	r = adminRequest(handler, stub, "POST", "/logout", `{}`)
	r.Header.Set("X-CSRF-Token", handler.csrf(previous))
	requireStatus(t, run(handler, r), 403)
}

func TestPrincipalAndSessionChannels(t *testing.T) {
	handler, stub, _ := fixture(t)
	r := request("GET", "/api/v2/auth/session?access_token="+string(stub.session.AccessToken), "")
	requireStatus(t, run(handler, r), 401)
	r = request("GET", "/api/v2/auth/session", "")
	r.AddCookie(&http.Cookie{Name: accessCookie, Value: string(stub.session.AccessToken)})
	requireStatus(t, run(handler, r), 401)
	r = request("GET", "/api/v2/auth/session", "")
	r.Header.Set("Authorization", "Bearer "+string(stub.session.AccessToken))
	requireStatus(t, run(handler, r), 200)
	if stub.scope != "desktop" {
		t.Fatal("桌面范围被改变")
	}
	r = adminRequest(handler, stub, "GET", "/session", "")
	r.Header.Set("Authorization", "Bearer "+string(stub.session.AccessToken))
	requireStatus(t, run(handler, r), 400)
	r = adminRequest(handler, stub, "POST", "/logout", `{}`)
	result := run(handler, r)
	requireStatus(t, result, 204)
	if stub.principal.UserID != stub.session.Principal.UserID {
		t.Fatal("未沿用认证结果中的用户")
	}
	for _, cookie := range result.Result().Cookies() {
		if cookie.MaxAge != -1 {
			t.Fatal("退出未清除管理凭据")
		}
	}
	if len(result.Result().Cookies()) != 2 {
		t.Fatal("退出仅清除了部分 Cookie")
	}
}

func TestStrictJSONAndErrorRedaction(t *testing.T) {
	for _, body := range []string{`{"scope":"admin"}`, `{"user_id":"injected"}`, `{} {}`, `null`, `[]`, strings.Repeat("a", maxBodyBytes+1), string([]byte{'{', 0xff, '}'})} {
		handler, stub, _ := fixture(t)
		requireStatus(t, run(handler, request("POST", "/api/v2/auth/login", body)), 400)
		if len(stub.calls) != 0 {
			t.Fatal("无效 JSON 已进入身份服务")
		}
	}
	handler, stub, logs := fixture(t)
	r := request("POST", "/api/v2/auth/login", `{}`)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requireStatus(t, run(handler, r), 415)
	sensitive := randomValue(t)
	stub.err = fmt.Errorf("%s: %w", sensitive, identity.ErrInvalidCredentials)
	result := run(handler, request("POST", "/api/v2/auth/login", `{}`))
	requireStatus(t, result, 401)
	if strings.Contains(result.Body.String(), sensitive) || strings.Contains(logs.String(), sensitive) {
		t.Fatal("认证错误暴露敏感上下文")
	}
	stub.err = errors.New(sensitive)
	result = run(handler, request("POST", "/api/v2/auth/login", `{}`))
	requireStatus(t, result, 503)
	if strings.Contains(result.Body.String(), sensitive) || strings.Contains(logs.String(), sensitive) || !strings.Contains(logs.String(), "request_id") {
		t.Fatal("内部错误未脱敏或缺少定位标识")
	}
}

func TestRemainingRoutesCarryVerifiedIdentity(t *testing.T) {
	for _, scenario := range []struct {
		method, path, body, call string
		status                   int
	}{
		{"POST", "/api/v2/auth/activate", `{}`, "activate", 204},
		{"GET", "/api/v2/auth/admin-setup", ``, "administrators-exist", 200},
		{"POST", "/api/v2/auth/admin-setup", `{"username":"first-admin","display_name":"首位管理员","password":"` + strings.Repeat("a", 16) + `"}`, "create-first-administrator", 201},
		{"POST", "/api/v2/auth/refresh", `{}`, "refresh", 200},
		{"POST", "/api/v2/auth/password", `{}`, "password", 204},
		{"POST", "/api/v2/auth/verify", `{}`, "verify", 204},
		{"GET", "/api/v2/auth/devices?limit=10", "", "devices", 200},
		{"DELETE", "/api/v2/auth/devices/" + ids.New(), "", "revoke", 204},
	} {
		t.Run(scenario.call, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			r := request(scenario.method, scenario.path, scenario.body)
			r.Header.Set("Authorization", "Bearer "+string(stub.session.AccessToken))
			requireStatus(t, run(handler, r), scenario.status)
			if len(stub.calls) == 0 || stub.calls[len(stub.calls)-1] != scenario.call {
				t.Fatal("未调用对应身份操作")
			}
			if stub.principal.UserID != "" && stub.principal.UserID != stub.session.Principal.UserID {
				t.Fatal("请求替换了已验证身份")
			}
		})
	}
}

func TestHTTPSCookieRoundTrip(t *testing.T) {
	handler, stub, _ := fixture(t)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	call := func(method, path, csrf string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", handler.options.AdminOrigin)
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("HTTPS 认证请求失败")
		}
		return response
	}
	response := call("POST", "/api/v2/admin/auth/login", "")
	var login struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err = json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal("管理登录响应格式错误")
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || login.CSRFToken == "" {
		t.Fatal("HTTPS 登录失败")
	}
	response = call("GET", "/api/v2/admin/auth/session", "")
	if _, err = io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatal("浏览器 Cookie 未随安全请求发送")
	}
	response = call("POST", "/api/v2/admin/auth/logout", login.CSRFToken)
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 204 || stub.principal.UserID != stub.session.Principal.UserID {
		t.Fatal("HTTPS 退出未生效")
	}
	response = call("GET", "/api/v2/admin/auth/session", "")
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 401 {
		t.Fatal("退出后仍发送管理 Cookie")
	}
}
