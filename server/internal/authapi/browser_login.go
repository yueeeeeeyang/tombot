package authapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"

	"io.yueyang/tom/internal/identity"
)

// 桌面端先登记回调，系统浏览器验证网页会话并批准授权，桌面端再用
// 一次性授权码与 PKCE 换取会话。密码与会话凭据不进入地址栏或页面脚本。

// browserStart 返回授权页地址；桌面端用系统浏览器打开它。
func (handler *Handler) browserStart(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RedirectURI   string `json:"redirect_uri"`
		State         string `json:"state"`
		CodeChallenge string `json:"code_challenge"`
		DeviceName    string `json:"device_name"`
		Platform      string `json:"platform"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	authorization, err := handler.identity.BeginBrowserAuthorization(r.Context(), identity.BrowserAuthorizationRequest{
		RedirectURI:   request.RedirectURI,
		State:         request.State,
		CodeChallenge: request.CodeChallenge,
		DeviceName:    request.DeviceName,
		Platform:      request.Platform,
	})
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	query := url.Values{}
	query.Set("authorization", authorization.ID)
	query.Set("state", authorization.State)
	query.Set("token", handler.browserToken(authorization.ID, authorization.State))
	handler.respond(w, r, http.StatusOK, struct {
		AuthorizationURL string `json:"authorization_url"`
		ExpiresAt        string `json:"expires_at"`
	}{"/authorize?" + query.Encode(), authorization.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")})
}

// browserToken 绑定授权编号与状态，防止他人在浏览器里替换表单目标。
func (handler *Handler) browserToken(authorizationID, state string) string {
	mac := hmac.New(sha256.New, handler.options.CSRFKey)
	mac.Write([]byte(authorizationID))
	mac.Write([]byte("|"))
	mac.Write([]byte(state))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (handler *Handler) validBrowserToken(authorizationID, state, token string) bool {
	expected := handler.browserToken(authorizationID, state)
	return hmac.Equal([]byte(expected), []byte(token))
}

// authorizationPage 只处理桌面授权；普通网页登录不依赖授权编号或回调参数。
func (handler *Handler) authorizationPage(w http.ResponseWriter, r *http.Request) {
	target := loginTarget{ID: r.URL.Query().Get("authorization"), State: r.URL.Query().Get("state"), Token: r.URL.Query().Get("token")}
	authorization, ok := handler.pendingLogin(w, r, target)
	if !ok {
		return
	}
	target.RedirectURI = authorization.RedirectURI
	if authorization.IsApproved {
		handler.writeLoginPage(w, r, http.StatusOK, "授权已提交", "请回到桌面客户端继续，本页面可以关闭。", "", "")
		return
	}
	principal, sessionToken, err := handler.webPrincipal(r)
	if err == nil {
		body := `<section class="account-summary"><span class="eyebrow">当前账号</span><strong>` + html.EscapeString(principal.DisplayName) + `</strong><span>@` + html.EscapeString(principal.Username) + `</span></section>` +
			`<p class="authorization-device">允许 <strong>` + html.EscapeString(authorization.DeviceName) + `</strong> 登录你的 TomBot 账号。</p>` +
			`<form method="post" action="/api/v2/auth/browser/approve">` + target.fields() + hiddenField("mode", "authorize") +
			hiddenField("csrf", handler.webCSRF("approve:"+target.ID+":"+target.State, sessionToken)) +
			`<button class="primary-button" type="submit">授权客户端</button></form><a class="page-link" href="/account">查看个人信息或退出登录</a>`
		handler.writeLoginPage(w, r, http.StatusOK, "授权 TomBot 客户端", "你已登录，确认授权后即可返回客户端。", body, target.RedirectURI)
		return
	}
	if !errors.Is(err, identity.ErrInvalidSession) {
		handler.serviceError(w, r, err)
		return
	}
	handler.writePasswordPage(w, r, http.StatusOK, "登录 TomBot", "登录后将授权此客户端，并在浏览器中保持登录。", target)
}

func (handler *Handler) pendingLogin(w http.ResponseWriter, r *http.Request, target loginTarget) (identity.Authorization, bool) {
	if !handler.validBrowserToken(target.ID, target.State, target.Token) {
		handler.writeLoginPage(w, r, http.StatusBadRequest, "登录链接无效", "请回到桌面客户端重新发起登录。", "", "")
		return identity.Authorization{}, false
	}
	authorization, err := handler.identity.PendingAuthorization(r.Context(), target.ID, target.State)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidAuthorization) || errors.Is(err, identity.ErrInvalidInput) {
			handler.writeLoginPage(w, r, http.StatusGone, "登录链接已失效", "请回到桌面客户端重新发起登录。", "", "")
		} else {
			handler.serviceError(w, r, err)
		}
		return identity.Authorization{}, false
	}
	return authorization, true
}

// browserApprove 复用网页会话并显式确认授权；未登录时先完成同一条普通用户登录链路。
func (handler *Handler) browserApprove(w http.ResponseWriter, r *http.Request) {
	if !handler.parseWebForm(w, r) {
		return
	}
	target := loginTarget{ID: r.PostForm.Get("authorization"), State: r.PostForm.Get("state"), Token: r.PostForm.Get("token")}
	authorization, ok := handler.pendingLogin(w, r, target)
	if !ok {
		return
	}
	target.RedirectURI = authorization.RedirectURI
	var principal identity.Principal
	if r.PostForm.Get("mode") == "authorize" {
		var sessionToken string
		var err error
		principal, sessionToken, err = handler.webPrincipal(r)
		if err != nil {
			if errors.Is(err, identity.ErrInvalidSession) {
				handler.writePasswordPage(w, r, http.StatusUnauthorized, "登录已过期", "请重新登录后继续授权。", target)
			} else {
				handler.serviceError(w, r, err)
			}
			return
		}
		if !handler.checkWebCSRF(w, r, "approve:"+target.ID+":"+target.State, sessionToken) {
			return
		}
	} else if r.PostForm.Get("mode") == "login" {
		var ok bool
		principal, ok = handler.passwordLogin(w, r, target)
		if !ok {
			return
		}
	} else {
		handler.serviceError(w, r, identity.ErrInvalidInput)
		return
	}
	code, err := handler.identity.ApproveBrowserAuthorization(r.Context(), target.ID, principal, currentAttempt(r))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	location, err := url.Parse(authorization.RedirectURI)
	if err != nil || !identity.ValidateRedirectURI(authorization.RedirectURI) {
		handler.writeLoginPage(w, r, http.StatusInternalServerError, "回调地址无效", "请重新发起登录。", "", "")
		return
	}
	query := location.Query()
	query.Set("code", code)
	query.Set("state", target.State)
	location.RawQuery = query.Encode()
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, location.String(), http.StatusSeeOther)
}

// browserExchange 用授权码与 PKCE 校验串换取正常会话；桌面端只在这里拿到凭据。
func (handler *Handler) browserExchange(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		State        string `json:"state"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	session, err := handler.identity.ExchangeBrowserAuthorization(r.Context(), request.Code, request.CodeVerifier, request.State, currentAttempt(r))
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.writeSession(w, r, "desktop", session)
}

func (handler *Handler) writeLoginPage(w http.ResponseWriter, r *http.Request, status int, title, message, body, redirectURI string) {
	formAction := "'self'"
	if redirectURI != "" {
		callback, err := url.Parse(redirectURI)
		if err != nil || !identity.ValidateRedirectURI(redirectURI) {
			status, title, message, body = http.StatusInternalServerError, "回调地址无效", "请回到桌面客户端重新发起登录。", ""
		} else {
			// 浏览器也会检查表单响应的重定向，只放行已登记的回环来源及其端口。
			formAction += " " + callback.Scheme + "://" + callback.Host
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	// 表单提交需要保留同源 Origin；仅允许来源站点，授权查询参数不会进入 Referer。
	w.Header().Set("Referrer-Policy", "strict-origin")
	w.WriteHeader(status)
	var page strings.Builder
	page.WriteString("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>")
	page.WriteString(html.EscapeString(title))
	page.WriteString(`</title><link rel="stylesheet" href="/admin/assets/login.css"></head><body><main class="login-card"><img class="login-brand" src="/admin/assets/login-logo.png" alt="TomBot 猫咪头像" width="64" height="64"><h1>`)
	page.WriteString(html.EscapeString(title))
	page.WriteString("</h1>")
	if status >= http.StatusBadRequest {
		page.WriteString(`<p class="form-error" role="alert">`)
	} else {
		page.WriteString(`<p class="login-description">`)
	}
	page.WriteString(html.EscapeString(message))
	page.WriteString("</p>")
	page.WriteString(body)
	page.WriteString("</main></body></html>")
	if _, err := w.Write([]byte(page.String())); err != nil {
		handler.logger.WarnContext(r.Context(), "登录页响应未发送完成", "request_id", currentAttempt(r).RequestID)
	}
}
