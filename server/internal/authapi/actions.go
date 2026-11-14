package authapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/identity"
)

func (handler *Handler) activate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ActivationToken string `json:"activation_token"`
		Password        string `json:"password"`
		Code            string `json:"code"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	if err := handler.identity.Activate(r.Context(), request.ActivationToken, request.Password, request.Code, currentAttempt(r)); err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, http.StatusNoContent, nil)
}

// adminSetupStatus 只回答平台是否已有管理员，供登录页决定是否展示首建入口。
// 不透露管理员数量、用户名或状态；该接口不需要会话，也不改变任何状态。
func (handler *Handler) adminSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		handler.failure(w, r, 400, "INPUT_INVALID", "此接口不接受查询参数")
		return
	}
	exists, err := handler.identity.AdministratorsExist(r.Context())
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, struct {
		Required bool `json:"required"`
	}{!exists})
}

// createFirstAdministrator 只在平台没有管理员时可用，由创建者当场设定口令。
// 动态验证器密钥只在本次响应中返回一次，服务端不保存明文。
func (handler *Handler) createFirstAdministrator(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	setup, err := handler.identity.CreateFirstAdministrator(r.Context(), request.Username, request.DisplayName, request.Password, currentAttempt(r))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 201, setup)
}

func (handler *Handler) login(scope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Username   string `json:"username"`
			Password   string `json:"password"`
			Code       string `json:"code"`
			DeviceName string `json:"device_name"`
			Platform   string `json:"platform"`
		}
		if !handler.decode(w, r, &request) {
			return
		}
		platform := request.Platform
		if scope == "admin" {
			platform = "web"
		}
		session, err := handler.identity.Login(r.Context(), identity.LoginRequest{Username: request.Username, Password: request.Password, Code: request.Code, DeviceName: request.DeviceName, Platform: platform, Scope: scope}, currentAttempt(r))
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
		handler.writeSession(w, r, scope, session)
	}
}

func (handler *Handler) refresh(scope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		if scope == "admin" {
			if !handler.checkCSRF(w, r) {
				return
			}
			var request struct{}
			if !handler.decode(w, r, &request) {
				return
			}
			value, err := cookieValue(r, refreshCookie)
			if err != nil {
				handler.serviceError(w, r, err)
				return
			}
			token = value
		} else {
			var request struct {
				RefreshToken string `json:"refresh_token"`
			}
			if !handler.decode(w, r, &request) {
				return
			}
			token = request.RefreshToken
		}
		session, err := handler.identity.Refresh(r.Context(), token, scope, currentAttempt(r))
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
		handler.writeSession(w, r, scope, session)
	}
}

// refreshContext 在访问 Cookie 到期后仍允许同源页面读取续期校验值，凭据本身始终留在 HttpOnly Cookie。
func (handler *Handler) refreshContext(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
		handler.failure(w, r, 400, "AUTH_INPUT_INVALID", "续期校验请求无效")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		handler.failure(w, r, 403, "ORIGIN_INVALID", "管理操作必须来自管理平台")
		return
	}
	refresh, err := cookieValue(r, refreshCookie)
	if err == nil {
		err = handler.identity.ValidateRefresh(r.Context(), refresh, "admin")
	}
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, struct {
		CSRFToken identity.Credential `json:"csrf_token"`
	}{identity.Credential(handler.csrf(refresh))})
}

func (handler *Handler) session(scope string) principalAction {
	return func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		if scope == "desktop" {
			handler.respond(w, r, 200, struct {
				Principal identity.Principal `json:"principal"`
			}{principal})
			return
		}
		refresh, err := cookieValue(r, refreshCookie)
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
		handler.respond(w, r, 200, struct {
			Principal       identity.Principal  `json:"principal"`
			CSRFToken       identity.Credential `json:"csrf_token"`
			AccessExpiresAt time.Time           `json:"access_expires_at"`
			ExpiresAt       time.Time           `json:"expires_at"`
		}{principal, identity.Credential(handler.csrf(refresh)), principal.AccessExpiresAt, principal.ExpiresAt})
	}
}

func (handler *Handler) logout(scope string) principalAction {
	return func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		var request struct{}
		if !handler.decode(w, r, &request) {
			return
		}
		if err := handler.identity.Logout(r.Context(), principal, currentAttempt(r).RequestID); err != nil {
			handler.serviceError(w, r, err)
			return
		}
		if scope == "admin" {
			handler.clearCookies(w)
		}
		handler.respond(w, r, http.StatusNoContent, nil)
	}
}

func (handler *Handler) password(scope string) principalAction {
	return func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		var request struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
			Code            string `json:"code"`
		}
		if !handler.decode(w, r, &request) {
			return
		}
		if err := handler.identity.ChangePassword(r.Context(), principal, request.CurrentPassword, request.NewPassword, request.Code, currentAttempt(r)); err != nil {
			handler.serviceError(w, r, err)
			return
		}
		if scope == "admin" {
			handler.clearCookies(w)
		}
		handler.respond(w, r, http.StatusNoContent, nil)
	}
}

func (handler *Handler) verify(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
	var request struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	if err := handler.identity.Reauthenticate(r.Context(), principal, request.Password, request.Code, currentAttempt(r)); err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, http.StatusNoContent, nil)
}

func (handler *Handler) revokeDevice(scope string) principalAction {
	return func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		deviceID := chi.URLParam(r, "deviceID")
		if err := handler.identity.RevokeDevice(r.Context(), principal, deviceID, currentAttempt(r).RequestID); err != nil {
			handler.serviceError(w, r, err)
			return
		}
		if scope == "admin" && principal.DeviceID == deviceID {
			handler.clearCookies(w)
		}
		handler.respond(w, r, http.StatusNoContent, nil)
	}
}
