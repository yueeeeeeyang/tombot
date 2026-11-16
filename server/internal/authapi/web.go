package authapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"time"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
)

const webSessionCookie = "__Host-tombot-user"
const webFormCookie = "__Host-tombot-user-form"

type loginTarget struct{ ID, State, Token, RedirectURI string }

func hiddenField(name, value string) string {
	return `<input type="hidden" name="` + html.EscapeString(name) + `" value="` + html.EscapeString(value) + `">`
}

func (target loginTarget) fields() string {
	return hiddenField("authorization", target.ID) + hiddenField("state", target.State) + hiddenField("token", target.Token)
}

// webCSRF 按操作、当前浏览器及授权目标绑定表单，不能从另一个会话搬用。
func (handler *Handler) webCSRF(purpose, credential string) string {
	mac := hmac.New(sha256.New, handler.options.CSRFKey)
	mac.Write([]byte("tombot.web.form\x00" + purpose + "\x00" + credential))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (handler *Handler) checkWebCSRF(w http.ResponseWriter, r *http.Request, purpose, credential string) bool {
	if credential == "" || !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(handler.webCSRF(purpose, credential))) {
		handler.failure(w, r, http.StatusForbidden, "CSRF_INVALID", "页面已失效，请刷新后重试")
		return false
	}
	return true
}

func (handler *Handler) parseWebForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		handler.serviceError(w, r, identity.ErrInvalidInput)
		return false
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			handler.serviceError(w, r, identity.ErrInvalidInput)
			return false
		}
	}
	return true
}

func setWebCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	// Lax 允许从桌面打开顶层授权页时带入会话；所有写入另验 Origin 与绑定表单。
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires})
}

func (handler *Handler) webPrincipal(r *http.Request) (identity.Principal, string, error) {
	if r.Header.Get("Authorization") != "" {
		return identity.Principal{}, "", identity.ErrInvalidSession
	}
	token, err := cookieValue(r, webSessionCookie)
	if err != nil {
		return identity.Principal{}, "", err
	}
	principal, err := handler.identity.Authenticate(r.Context(), token, "web")
	return principal, token, err
}

func (handler *Handler) writePasswordPage(w http.ResponseWriter, r *http.Request, status int, title, message string, target loginTarget) {
	nonce, err := cookieValue(r, webFormCookie)
	if err != nil {
		nonce, err = secure.Token()
		if err != nil {
			handler.serviceError(w, r, err)
			return
		}
	}
	setWebCookie(w, webFormCookie, nonce, time.Now().Add(identity.AuthorizationLifetime))
	action := "/api/v2/web/auth/login"
	if target.ID != "" {
		action = "/api/v2/auth/browser/approve"
	}
	body := `<form method="post" action="` + action + `">` + target.fields() + hiddenField("mode", "login") +
		hiddenField("csrf", handler.webCSRF("login:"+target.ID+":"+target.State, nonce)) +
		`<label class="field">用户名<input name="username" autocomplete="username" autocapitalize="none" spellcheck="false" placeholder="输入用户名" maxlength="64" required></label>` +
		`<label class="field">密码<input name="password" type="password" autocomplete="current-password" placeholder="输入密码" maxlength="1024" required></label>` +
		`<button class="primary-button" type="submit">登录</button></form><a class="page-link" href="/activate">首次使用？激活账号</a>`
	handler.writeLoginPage(w, r, status, title, message, body, target.RedirectURI)
}

func (handler *Handler) passwordLogin(w http.ResponseWriter, r *http.Request, target loginTarget) (identity.Principal, bool) {
	nonce, err := cookieValue(r, webFormCookie)
	if err != nil || !handler.checkWebCSRF(w, r, "login:"+target.ID+":"+target.State, nonce) {
		if err != nil {
			handler.failure(w, r, http.StatusForbidden, "CSRF_INVALID", "页面已失效，请刷新后重试")
		}
		return identity.Principal{}, false
	}
	session, err := handler.identity.Login(r.Context(), identity.LoginRequest{Username: r.PostForm.Get("username"), Password: r.PostForm.Get("password"), DeviceName: "TomBot 网页", Platform: "web", Scope: "web"}, currentAttempt(r))
	if err != nil {
		status, message := http.StatusUnauthorized, "用户名或密码不正确，或账户不可用。"
		if errors.Is(err, identity.ErrRateLimited) {
			status, message = http.StatusTooManyRequests, "尝试次数过多，请稍后再试。"
		} else if errors.Is(err, identity.ErrDeviceLimit) {
			status, message = http.StatusConflict, "有效设备数量已达到上限，请先退出其他设备。"
		} else if !errors.Is(err, identity.ErrInvalidCredentials) && !errors.Is(err, identity.ErrInvalidInput) {
			handler.serviceError(w, r, err)
			return identity.Principal{}, false
		}
		handler.writePasswordPage(w, r, status, "登录失败", message, target)
		return identity.Principal{}, false
	}
	setWebCookie(w, webSessionCookie, string(session.AccessToken), session.ExpiresAt)
	return session.Principal, true
}

func (handler *Handler) webLogin(w http.ResponseWriter, r *http.Request) {
	if !handler.parseWebForm(w, r) {
		return
	}
	if _, ok := handler.passwordLogin(w, r, loginTarget{}); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// loginPage 可由普通浏览器独立访问；已登录时进入用户平台，不重新创建会话。
func (handler *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, err := handler.webPrincipal(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	} else if errors.Is(err, identity.ErrInvalidSession) {
		message := "登录你的账号，查看个人信息与用量管理。"
		if r.URL.Query().Get("password") == "changed" {
			message = "密码修改成功，所有设备已退出登录。请使用新密码重新登录。"
		}
		handler.writePasswordPage(w, r, http.StatusOK, "TomBot 用户平台", message, loginTarget{})
	} else {
		handler.serviceError(w, r, err)
	}
}

func (handler *Handler) webLogout(w http.ResponseWriter, r *http.Request) {
	if !handler.parseWebForm(w, r) {
		return
	}
	principal, token, err := handler.webPrincipal(r)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	if !handler.checkWebCSRF(w, r, "logout", token) {
		return
	}
	if err := handler.identity.Logout(r.Context(), principal, currentAttempt(r).RequestID); err != nil {
		handler.serviceError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: webSessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
