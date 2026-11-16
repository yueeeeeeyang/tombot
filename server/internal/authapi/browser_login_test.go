package authapi

import (
	"net/url"
	"strings"
	"testing"
)

func TestBrowserLoginPageSecurity(t *testing.T) {
	for _, test := range []struct {
		name, callback, origin string
		status                 int
	}{
		{"本机 IPv4", "http://127.0.0.1:49152/callback", "http://127.0.0.1:49152", 200},
		{"本机域名", "http://localhost:49153/callback", "http://localhost:49153", 200},
		{"路径不能注入策略", "http://127.0.0.1:49152/callback;script-src%20*", "http://127.0.0.1:49152", 200},
		{"外部地址", "https://outside.example.test/callback", "", 500},
		{"无效地址", "http://127.0.0.1:bad/callback", "", 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			stub.authorizationID, stub.authorizationState, stub.redirectURI = randomValue(t), randomValue(t), test.callback
			query := url.Values{"authorization": {stub.authorizationID}, "state": {stub.authorizationState}, "token": {handler.browserToken(stub.authorizationID, stub.authorizationState)}}
			result := run(handler, request("GET", "/authorize?"+query.Encode(), ""))
			requireStatus(t, result, test.status)
			action := "'self'"
			if test.origin != "" {
				action += " " + test.origin
			}
			policy := "default-src 'none'; style-src 'self'; img-src 'self'; form-action " + action + "; base-uri 'none'; frame-ancestors 'none'"
			if result.Header().Get("Content-Security-Policy") != policy || result.Header().Get("Referrer-Policy") != "strict-origin" {
				t.Fatal("表单来源或回环重定向策略无效")
			}
			if strings.Contains(result.Body.String(), "<form") != (test.status == 200) {
				t.Fatal("无效回调仍提供登录表单")
			}
			for _, marker := range []string{`class="login-card"`, `href="/admin/assets/login.css"`, `src="/admin/assets/login-logo.png"`} {
				if !strings.Contains(result.Body.String(), marker) {
					t.Fatal("登录页未使用统一品牌样式", marker)
				}
			}
			if strings.Contains(result.Body.String(), "<script") || strings.Contains(result.Body.String(), "<style") {
				t.Fatal("登录页引入了脚本或内联样式")
			}
		})
	}
}

func TestBrowserApproveRejectsForeignOrigin(t *testing.T) {
	for _, origin := range []string{"null", "https://outside.example.test"} {
		t.Run(origin, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			r := request("POST", "/api/v2/auth/browser/approve", "")
			r.Header.Set("Origin", origin)
			requireStatus(t, run(handler, r), 403)
			if len(stub.calls) != 0 {
				t.Fatal("非法来源进入了身份服务")
			}
		})
	}
}
