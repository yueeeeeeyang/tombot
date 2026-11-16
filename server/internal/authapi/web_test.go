package authapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
)

func TestWebFormRejectsCrossSiteAndUnboundRequests(t *testing.T) {
	for _, path := range []string{"/api/v2/web/auth/login", "/api/v2/web/auth/logout", "/api/v2/web/auth/profile", "/api/v2/web/auth/password", "/api/v2/auth/browser/approve"} {
		for _, origin := range []string{"", "null", "https://outside.example.test"} {
			handler, stub, _ := fixture(t)
			r := request("POST", path, "")
			r.Header.Set("Origin", origin)
			requireStatus(t, run(handler, r), 403)
			if len(stub.calls) != 0 {
				t.Fatal("非法来源进入身份服务")
			}
		}
	}
	for _, change := range []string{"missing-cookie", "wrong-cookie", "missing-csrf", "duplicate-csrf", "different-purpose"} {
		t.Run(change, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			nonce := randomValue(t)
			form := url.Values{"csrf": {handler.webCSRF("login::", nonce)}, "username": {"user"}, "password": {randomValue(t)}}
			switch change {
			case "missing-csrf":
				form.Del("csrf")
			case "duplicate-csrf":
				form.Add("csrf", form.Get("csrf"))
			case "wrong-cookie":
				nonce = randomValue(t)
			case "different-purpose":
				form.Set("csrf", handler.webCSRF("logout", nonce))
			}
			r := request("POST", "/api/v2/web/auth/login", form.Encode())
			r.Header.Set("Origin", "https://"+r.Host)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if change != "missing-cookie" {
				r.AddCookie(&http.Cookie{Name: webFormCookie, Value: nonce})
			}
			result := run(handler, r)
			if result.Code != 400 && result.Code != 403 {
				t.Fatal("未绑定请求没有拒绝", result.Code)
			}
			if len(stub.calls) != 0 {
				t.Fatal("未绑定请求进入身份服务")
			}
		})
	}
}

func TestStandaloneUserLoginDoesNotRequireDesktopAuthorization(t *testing.T) {
	for _, path := range []string{"/login", "/login?source=bookmark", "/login?authorization=invalid"} {
		handler, stub, _ := fixture(t)
		result := run(handler, request("GET", path, ""))
		requireStatus(t, result, http.StatusOK)
		for _, marker := range []string{"TomBot 用户平台", `action="/api/v2/web/auth/login"`, `name="username"`, `name="password"`} {
			if !strings.Contains(result.Body.String(), marker) {
				t.Fatal("独立用户登录页面缺少入口", path, marker)
			}
		}
		if len(stub.calls) != 0 || strings.Contains(result.Body.String(), `action="/api/v2/auth/browser/approve"`) {
			t.Fatal("普通网页登录仍依赖客户端授权")
		}
	}
	handler, _, _ := fixture(t)
	requireStatus(t, run(handler, request("GET", "/authorize", "")), http.StatusBadRequest)
}

func TestUserPlatformRequiresOwnSession(t *testing.T) {
	for _, path := range []string{"/", "/account", "/account/password"} {
		for _, mode := range []string{"anonymous", "administrator", "expired", "unavailable", "authenticated"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				handler, stub, _ := fixture(t)
				stub.session.Principal.Role = "user"
				r := request("GET", path, "")
				switch mode {
				case "administrator":
					r.AddCookie(&http.Cookie{Name: accessCookie, Value: string(stub.session.AccessToken)})
				case "expired", "unavailable", "authenticated":
					r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: string(stub.session.AccessToken)})
				}
				if mode == "expired" {
					stub.err = identity.ErrInvalidSession
				} else if mode == "unavailable" {
					stub.err = errors.New("测试数据库不可用")
				}
				result := run(handler, r)
				if mode == "authenticated" {
					requireStatus(t, result, 200)
					for _, marker := range []string{`class="user-shell"`, `class="navigation-footer"`, `aria-label="用户导航"`, `href="/account"`, `action="/api/v2/web/auth/logout"`} {
						if !strings.Contains(result.Body.String(), marker) {
							t.Fatal("用户平台布局或账户入口缺失", marker)
						}
					}
					if strings.Contains(result.Body.String(), "用量管理即将上线") != (path == "/") || strings.Contains(result.Body.String(), string(stub.session.AccessToken)) {
						t.Fatal("页面内容错误或泄露会话凭据")
					}
					if !strings.Contains(result.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || result.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("用户平台缺少页面保护")
					}
				} else if mode == "unavailable" {
					requireStatus(t, result, 503)
				} else {
					requireStatus(t, result, 303)
					if result.Header().Get("Location") != "/login" {
						t.Fatal("未登录用户没有进入独立登录页")
					}
				}
			})
		}
	}
}

func TestWebPagesEscapeIdentityAndKeepAdminSeparate(t *testing.T) {
	handler, stub, _ := fixture(t)
	stub.session.Principal.Role = "user"
	stub.session.Principal.DisplayName = `<script>unsafe()</script>`
	stub.session.Principal.Username = `<b>user</b>`
	r := request("GET", "/account", "")
	r.AddCookie(&http.Cookie{Name: accessCookie, Value: string(stub.session.AccessToken)})
	result := run(handler, r)
	requireStatus(t, result, 303)
	if result.Header().Get("Location") != "/login" || len(stub.calls) != 0 {
		t.Fatal("普通网页复用了管理 Cookie")
	}
	r = request("GET", "/account", "")
	r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: string(stub.session.AccessToken)})
	result = run(handler, r)
	requireStatus(t, result, 200)
	if strings.Contains(result.Body.String(), "<script>") || strings.Contains(result.Body.String(), "<b>user</b>") || !strings.Contains(result.Body.String(), "&lt;script&gt;") {
		t.Fatal("个人信息没有转义")
	}
	if stub.scope != "web" {
		t.Fatal("网页没有独立鉴权范围")
	}
	stub.err = errors.New("测试数据库不可用")
	result = run(handler, r)
	requireStatus(t, result, 503)
	if strings.Contains(result.Body.String(), "个人信息") {
		t.Fatal("基础设施错误被默认成功掩盖")
	}
}

func TestWebAuthorizationBindsCurrentSessionAndTarget(t *testing.T) {
	handler, stub, _ := fixture(t)
	stub.session.Principal.Role = "user"
	stub.authorizationID, stub.authorizationState, stub.redirectURI = randomValue(t), randomValue(t), "http://127.0.0.1:49152/callback"
	form := url.Values{"authorization": {stub.authorizationID}, "state": {stub.authorizationState}, "token": {handler.browserToken(stub.authorizationID, stub.authorizationState)}, "mode": {"authorize"}}
	// 另一个会话或授权目标的校验值即使有效，也不能用于当前批准。
	for _, csrf := range []string{handler.webCSRF("approve:"+stub.authorizationID+":"+stub.authorizationState, randomValue(t)), handler.webCSRF("approve:"+randomValue(t)+":"+stub.authorizationState, string(stub.session.AccessToken))} {
		stub.calls = nil
		form.Set("csrf", csrf)
		r := request("POST", "/api/v2/auth/browser/approve", form.Encode())
		r.Header.Set("Origin", "https://"+r.Host)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: string(stub.session.AccessToken)})
		requireStatus(t, run(handler, r), 403)
		for _, call := range stub.calls {
			if call == "browser-approve" {
				t.Fatal("跨会话或跨目标表单签发了授权码")
			}
		}
	}
	stub.err = identity.ErrInvalidSession
	result := run(handler, request("GET", "/account", ""))
	requireStatus(t, result, 303)
}
