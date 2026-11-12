// Package authapi 提供新用户认证接口；业务身份规则由 identity 唯一实现。
package authapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

const accessCookie = "__Host-tombot-access"
const refreshCookie = "__Host-tombot-refresh"
const maxBodyBytes = 16 << 10

// Identity 限定传输层所需的身份能力；实现不能通过 HTTP 参数选择数据库身份。
type Identity interface {
	Activate(context.Context, string, string, string, identity.Attempt) error
	BeginAdminEnrollment(context.Context, string, identity.Attempt) (identity.Credential, error)
	Login(context.Context, identity.LoginRequest, identity.Attempt) (identity.Session, error)
	Refresh(context.Context, string, string, identity.Attempt) (identity.Session, error)
	Authenticate(context.Context, string, string) (identity.Principal, error)
	Logout(context.Context, identity.Principal, string) error
	ChangePassword(context.Context, identity.Principal, string, string, string, identity.Attempt) error
	Reauthenticate(context.Context, identity.Principal, string, string, identity.Attempt) error
	ListDevices(context.Context, identity.Principal, string, int) ([]identity.Device, error)
	RevokeDevice(context.Context, identity.Principal, string, string) error
}

// Options 外部指定管理站点来源、受信反向代理、CSRF 专用密钥及单请求时限。
type Options struct {
	AdminOrigin    string
	TrustedProxies []netip.Prefix
	CSRFKey        []byte
	RequestTimeout time.Duration
}

// Handler 负责协议、Cookie 和 CSRF，不包含用户生命周期或权限判断的副本。
type Handler struct {
	identity Identity
	options  Options
	logger   *slog.Logger
	router   http.Handler
}

type requestContextKey struct{}
type requestContext struct{ attempt identity.Attempt }

// New 构建完整 v2 认证路由；缺少 HTTPS 来源、日志或安全配置时拒绝启动。
func New(service Identity, options Options, logger *slog.Logger) (*Handler, error) {
	origin, err := url.Parse(options.AdminOrigin)
	if err != nil || origin == nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" ||
		service == nil || logger == nil || len(options.CSRFKey) != 32 || options.RequestTimeout < time.Second || options.RequestTimeout > time.Minute {
		return nil, errors.New("认证 HTTP 配置无效")
	}
	for _, prefix := range options.TrustedProxies {
		if !prefix.IsValid() || prefix.Bits() == 0 {
			return nil, errors.New("受信代理范围无效或过宽")
		}
	}
	options.AdminOrigin = strings.TrimSuffix(options.AdminOrigin, "/")
	options.CSRFKey = append([]byte(nil), options.CSRFKey...)
	options.TrustedProxies = append([]netip.Prefix(nil), options.TrustedProxies...)
	handler := &Handler{identity: service, options: options, logger: logger}
	router := chi.NewRouter()
	router.Use(handler.transport)
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, http.StatusNotFound, "NOT_FOUND", "接口不存在")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "请求方式不支持")
	})
	router.Post("/api/v2/auth/activate", handler.activate)
	router.Post("/api/v2/auth/admin-enrollment", handler.enrollment)
	for _, scope := range []string{"desktop", "admin"} {
		prefix := "/api/v2/auth"
		if scope == "admin" {
			prefix = "/api/v2/admin/auth"
		}
		router.Post(prefix+"/login", handler.login(scope))
		router.Post(prefix+"/refresh", handler.refresh(scope))
		router.Get(prefix+"/session", handler.withPrincipal(scope, handler.session(scope)))
		router.Post(prefix+"/logout", handler.withPrincipal(scope, handler.logout(scope)))
		router.Post(prefix+"/password", handler.withPrincipal(scope, handler.password(scope)))
		router.Post(prefix+"/verify", handler.withPrincipal(scope, handler.verify))
		router.Get(prefix+"/devices", handler.withPrincipal(scope, handler.devices))
		router.Delete(prefix+"/devices/{deviceID}", handler.withPrincipal(scope, handler.revokeDevice(scope)))
	}
	handler.router = router
	return handler, nil
}

// ServeHTTP 执行已配置的认证路由；必须挂接到具有读取、写入及空闲时限的 HTTPS 服务。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler.router.ServeHTTP(w, r)
}

func (handler *Handler) transport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), handler.options.RequestTimeout)
		defer cancel()
		requestID := ids.New()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		source, secure, err := handler.source(r)
		r = r.WithContext(context.WithValue(ctx, requestContextKey{}, requestContext{attempt: identity.Attempt{Source: source, RequestID: requestID}}))
		if err != nil {
			handler.failure(w, r, 400, "SOURCE_INVALID", "请求来源信息无效")
			return
		}
		if !secure {
			handler.failure(w, r, 400, "HTTPS_REQUIRED", "认证接口必须使用 HTTPS")
			return
		}
		if len(r.Header.Values("Origin")) > 1 {
			handler.failure(w, r, 403, "ORIGIN_INVALID", "请求来源不允许")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != handler.options.AdminOrigin {
			handler.failure(w, r, 403, "ORIGIN_INVALID", "请求来源不允许")
			return
		}
		isAdmin := strings.HasPrefix(r.URL.Path, "/api/v2/admin/")
		if isAdmin && r.Method != "GET" && (origin != handler.options.AdminOrigin || r.Header.Get("Sec-Fetch-Site") == "cross-site" || r.Header.Get("Sec-Fetch-Site") == "same-site") {
			handler.failure(w, r, 403, "ORIGIN_INVALID", "管理操作必须来自管理平台")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// source 只接受直连地址或部署明确列出的代理；代理必须覆盖单一客户端地址与原始协议。
func (handler *Handler) source(r *http.Request) (netip.Addr, bool, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false, identity.ErrInvalidInput
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false, identity.ErrInvalidInput
	}
	address = address.Unmap()
	isSecure := r.TLS != nil
	for _, prefix := range handler.options.TrustedProxies {
		if !prefix.Contains(address) {
			continue
		}
		if len(r.Header.Values("X-Forwarded-For")) != 1 || len(r.Header.Values("X-Forwarded-Proto")) != 1 || r.Header.Get("X-Forwarded-Proto") != "https" {
			return netip.Addr{}, false, identity.ErrInvalidInput
		}
		forwarded, err := netip.ParseAddr(r.Header.Get("X-Forwarded-For"))
		if err != nil || forwarded.IsUnspecified() {
			return netip.Addr{}, false, identity.ErrInvalidInput
		}
		return forwarded.Unmap(), true, nil
	}
	return address, isSecure, nil
}

func currentAttempt(r *http.Request) identity.Attempt {
	return r.Context().Value(requestContextKey{}).(requestContext).attempt
}

func (handler *Handler) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if failure := httpjson.Decode(r, target, maxBodyBytes); failure != nil {
		handler.failure(w, r, failure.Status, failure.Code, failure.Message)
		return false
	}
	return true
}

func cookieValue(r *http.Request, name string) (string, error) {
	var value string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			value = cookie.Value
			count++
		}
	}
	if count != 1 || len(value) != 43 {
		return "", identity.ErrInvalidSession
	}
	return value, nil
}

func (handler *Handler) csrf(refresh string) string {
	mac := hmac.New(sha256.New, handler.options.CSRFKey)
	if _, err := mac.Write([]byte("tombot.admin.csrf\x00" + refresh)); err != nil {
		panic("计算管理请求校验失败")
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (handler *Handler) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	refresh, err := cookieValue(r, refreshCookie)
	provided := r.Header.Get("X-CSRF-Token")
	if err != nil || len(r.Header.Values("X-CSRF-Token")) != 1 || !hmac.Equal([]byte(provided), []byte(handler.csrf(refresh))) {
		handler.failure(w, r, 403, "CSRF_INVALID", "管理请求校验失败，请刷新页面后重试")
		return false
	}
	return true
}

type principalAction func(http.ResponseWriter, *http.Request, identity.Principal)

func (handler *Handler) withPrincipal(scope string, action principalAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		var err error
		if scope == "admin" {
			if r.Header.Get("Authorization") != "" {
				handler.failure(w, r, 400, "AUTH_CHANNEL_INVALID", "管理平台仅接受 Cookie 会话")
				return
			}
			if r.Method != "GET" && !handler.checkCSRF(w, r) {
				return
			}
			token, err = cookieValue(r, accessCookie)
		} else {
			header := r.Header.Get("Authorization")
			if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(header, "Bearer ") {
				err = identity.ErrInvalidSession
			} else {
				token = strings.TrimPrefix(header, "Bearer ")
			}
		}
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
		principal, err := handler.identity.Authenticate(r.Context(), token, scope)
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
		action(w, r, principal)
	}
}

func (handler *Handler) writeSession(w http.ResponseWriter, r *http.Request, scope string, session identity.Session) {
	if scope == "desktop" {
		handler.respond(w, r, 200, session)
		return
	}
	handler.setCookie(w, accessCookie, string(session.AccessToken), session.AccessExpiresAt)
	handler.setCookie(w, refreshCookie, string(session.RefreshToken), session.ExpiresAt)
	handler.respond(w, r, 200, struct {
		Principal       identity.Principal  `json:"principal"`
		CSRFToken       identity.Credential `json:"csrf_token"`
		AccessExpiresAt time.Time           `json:"access_expires_at"`
		ExpiresAt       time.Time           `json:"expires_at"`
	}{session.Principal, identity.Credential(handler.csrf(string(session.RefreshToken))), session.AccessExpiresAt, session.ExpiresAt})
}

func (handler *Handler) setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Expires: expires, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (handler *Handler) clearCookies(w http.ResponseWriter) {
	for _, name := range []string{accessCookie, refreshCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
}

func (handler *Handler) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidInput):
		handler.failure(w, r, 400, "AUTH_INPUT_INVALID", identity.ErrInvalidInput.Error())
	case errors.Is(err, identity.ErrInvalidActivation):
		handler.failure(w, r, 400, "ACTIVATION_INVALID", identity.ErrInvalidActivation.Error())
	case errors.Is(err, identity.ErrInvalidCredentials):
		handler.failure(w, r, 401, "AUTH_INVALID", identity.ErrInvalidCredentials.Error())
	case errors.Is(err, identity.ErrInvalidSession):
		handler.failure(w, r, 401, "SESSION_INVALID", identity.ErrInvalidSession.Error())
	case errors.Is(err, identity.ErrForbidden):
		handler.failure(w, r, 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error())
	case errors.Is(err, identity.ErrRateLimited):
		handler.failure(w, r, 429, "AUTH_RATE_LIMITED", identity.ErrRateLimited.Error())
	case errors.Is(err, identity.ErrDeviceLimit):
		handler.failure(w, r, 409, "DEVICE_LIMIT", identity.ErrDeviceLimit.Error())
	default:
		handler.logger.ErrorContext(r.Context(), "认证服务调用失败", "request_id", currentAttempt(r).RequestID)
		handler.failure(w, r, 503, "IDENTITY_UNAVAILABLE", "认证服务暂不可用，请稍后重试")
	}
}

func (handler *Handler) failure(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	handler.respond(w, r, status, struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}{code, message, currentAttempt(r).RequestID})
}

func (handler *Handler) respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if value == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		handler.logger.WarnContext(r.Context(), "认证响应写入失败", "request_id", currentAttempt(r).RequestID)
	}
}

func (handler *Handler) devices(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
	limit := 50
	query := r.URL.Query()
	for key, values := range query {
		if (key != "before" && key != "limit") || len(values) != 1 {
			handler.serviceError(w, r, identity.ErrInvalidInput)
			return
		}
	}
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			handler.serviceError(w, r, identity.ErrInvalidInput)
			return
		}
		limit = parsed
	}
	devices, err := handler.identity.ListDevices(r.Context(), principal, query.Get("before"), limit)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, struct {
		Devices []identity.Device `json:"devices"`
	}{devices})
}
