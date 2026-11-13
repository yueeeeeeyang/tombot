package authapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"html"
	"net/http"
	"net/url"
	"strings"

	"io.yueyang/tom/internal/identity"
)

// 浏览器登录的三段式：桌面端先登记回调，系统浏览器完成口令与动态验证，
// 桌面端再用一次性授权码换取会话。凭据从不经过浏览器地址栏或页面脚本。

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
	}{"/login?" + query.Encode(), authorization.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")})
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

// loginPage 输出最小登录表单；不使用脚本，避免把凭据暴露给页面代码。
func (handler *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	authorizationID := r.URL.Query().Get("authorization")
	state := r.URL.Query().Get("state")
	token := r.URL.Query().Get("token")
	if !handler.validBrowserToken(authorizationID, state, token) {
		handler.writeLoginPage(w, r, http.StatusBadRequest, "登录链接无效", "", "")
		return
	}
	authorization, err := handler.identity.PendingAuthorization(r.Context(), authorizationID, state)
	if err != nil {
		handler.writeLoginPage(w, r, http.StatusGone, "登录链接已失效", "请回到桌面客户端重新发起登录。", "")
		return
	}
	if authorization.IsApproved {
		handler.writeLoginPage(w, r, http.StatusOK, "登录已提交", "请回到桌面客户端继续，本页面可以关闭。", "")
		return
	}
	handler.writeLoginPage(w, r, http.StatusOK, "登录 TomBot", "在桌面客户端登录。凭据只提交到这台服务。", handler.loginForm(authorizationID, state, token))
}

// browserApprove 处理登录表单；成功后把一次性授权码重定向回桌面端回调地址。
func (handler *Handler) browserApprove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		handler.writeLoginPage(w, r, http.StatusBadRequest, "表单无法解析", "", "")
		return
	}
	authorizationID := r.PostForm.Get("authorization")
	state := r.PostForm.Get("state")
	token := r.PostForm.Get("token")
	if !handler.validBrowserToken(authorizationID, state, token) {
		handler.writeLoginPage(w, r, http.StatusBadRequest, "登录链接无效", "", "")
		return
	}
	code, err := handler.identity.ApproveBrowserAuthorization(r.Context(), authorizationID, r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("code"), currentAttempt(r))
	if err != nil {
		// 授权被拒时停留在本页，保留用户输入的用户名，清空口令与动态码。
		message := "用户名或密码不正确，或账户不可用。"
		if err == identity.ErrRateLimited {
			message = "尝试次数过多，请稍后再试。"
		}
		handler.writeLoginPage(w, r, http.StatusUnauthorized, "登录失败", message, handler.loginForm(authorizationID, state, token))
		return
	}
	authorization, err := handler.identity.PendingAuthorization(r.Context(), authorizationID, state)
	if err != nil {
		handler.writeLoginPage(w, r, http.StatusGone, "登录链接已失效", "请回到桌面客户端重新发起登录。", "")
		return
	}
	location, err := url.Parse(authorization.RedirectURI)
	if err != nil {
		handler.writeLoginPage(w, r, http.StatusInternalServerError, "回调地址无效", "", "")
		return
	}
	query := location.Query()
	query.Set("code", code)
	query.Set("state", state)
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

func (handler *Handler) loginForm(authorizationID, state, token string) string {
	escape := html.EscapeString
	return `<form method="post" action="/api/v2/auth/browser/approve">` +
		`<input type="hidden" name="authorization" value="` + escape(authorizationID) + `">` +
		`<input type="hidden" name="state" value="` + escape(state) + `">` +
		`<input type="hidden" name="token" value="` + escape(token) + `">` +
		`<label>用户名<input name="username" autocomplete="username" required></label>` +
		`<label>密码<input name="password" type="password" autocomplete="current-password" required></label>` +
		`<label>动态验证码（管理员账户填写）<input name="code" autocomplete="one-time-code"></label>` +
		`<button type="submit">登录</button>` +
		`</form>`
}

func (handler *Handler) writeLoginPage(w http.ResponseWriter, r *http.Request, status int, title, message, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	var page strings.Builder
	page.WriteString("<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>")
	page.WriteString(html.EscapeString(title))
	page.WriteString("</title><style>body{font-family:system-ui,sans-serif;max-width:22rem;margin:12vh auto;padding:0 1.25rem}label{display:block;margin:.75rem 0}input{display:block;width:100%;padding:.5rem;margin-top:.25rem;box-sizing:border-box}button{width:100%;padding:.6rem;margin-top:1rem}form{display:block}</style><h1>")
	page.WriteString(html.EscapeString(title))
	page.WriteString("</h1><p>")
	page.WriteString(html.EscapeString(message))
	page.WriteString("</p>")
	page.WriteString(body)
	page.WriteString("</body></html>")
	if _, err := w.Write([]byte(page.String())); err != nil {
		handler.logger.WarnContext(r.Context(), "登录页响应未发送完成", "request_id", currentAttempt(r).RequestID)
	}
}
