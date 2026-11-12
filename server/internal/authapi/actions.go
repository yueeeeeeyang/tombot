package authapi

import (
	"net/http"

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

func (handler *Handler) enrollment(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ActivationToken string `json:"activation_token"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	secret, err := handler.identity.BeginAdminEnrollment(r.Context(), request.ActivationToken, currentAttempt(r))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, struct {
		Secret    identity.Credential `json:"secret"`
		Period    int                 `json:"period"`
		Digits    int                 `json:"digits"`
		Algorithm string              `json:"algorithm"`
	}{secret, 30, 6, "SHA1"})
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
			Principal identity.Principal  `json:"principal"`
			CSRFToken identity.Credential `json:"csrf_token"`
		}{principal, identity.Credential(handler.csrf(refresh))})
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
