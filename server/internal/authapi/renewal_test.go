package authapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"io.yueyang/tom/internal/identity"
)

func TestAdminSessionIncludesActualExpiry(t *testing.T) {
	handler, stub, _ := fixture(t)
	result := run(handler, adminRequest(handler, stub, "GET", "/session", ""))
	requireStatus(t, result, 200)
	var response struct {
		AccessExpiresAt time.Time `json:"access_expires_at"`
		ExpiresAt       time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.AccessExpiresAt.Equal(stub.session.AccessExpiresAt) || !response.ExpiresAt.Equal(stub.session.ExpiresAt) {
		t.Fatal("恢复会话没有返回真实到期时间")
	}
}

func TestAdminRenewalWithoutAccessCookie(t *testing.T) {
	handler, stub, _ := fixture(t)
	makeRequest := func(method, path string) *http.Request {
		r := request(method, "/api/v2/admin/auth"+path, `{}`)
		r.Header.Set("Origin", handler.options.AdminOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.AddCookie(&http.Cookie{Name: refreshCookie, Value: string(stub.session.RefreshToken)})
		return r
	}
	requireStatus(t, run(handler, makeRequest("GET", "/session")), 401)
	result := run(handler, makeRequest("GET", "/refresh-context"))
	requireStatus(t, result, 200)
	var context struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &context); err != nil || context.CSRFToken == "" {
		t.Fatal("缺少续期校验值")
	}
	if len(result.Result().Cookies()) != 0 || stub.scope != "admin" || len(stub.calls) != 1 || stub.calls[0] != "validate-refresh" {
		t.Fatal("只读续期准备改变了会话或作用域")
	}
	r := makeRequest("POST", "/refresh")
	requireStatus(t, run(handler, r), 403)
	r = makeRequest("POST", "/refresh")
	r.Header.Set("X-CSRF-Token", context.CSRFToken)
	requireStatus(t, run(handler, r), 200)
	if stub.calls[len(stub.calls)-1] != "refresh" {
		t.Fatal("访问 Cookie 到期后未能续期")
	}

	for _, scenario := range []string{"跨站", "同站其他来源", "外部来源", "重复 Cookie", "无 Cookie", "已失效", "额外查询", "认证头"} {
		t.Run(scenario, func(t *testing.T) {
			r := makeRequest("GET", "/refresh-context")
			stub.err = nil
			status := 403
			switch scenario {
			case "跨站":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "同站其他来源":
				r.Header.Set("Sec-Fetch-Site", "same-site")
			case "外部来源":
				r.Header.Set("Origin", "https://other.example.test")
			case "重复 Cookie":
				r.AddCookie(&http.Cookie{Name: refreshCookie, Value: randomValue(t)})
				status = 401
			case "无 Cookie":
				r.Header.Del("Cookie")
				status = 401
			case "已失效":
				stub.err = identity.ErrInvalidSession
				status = 401
			case "额外查询":
				r.URL.RawQuery = "unexpected=1"
				status = 400
			case "认证头":
				r.Header.Set("Authorization", "Bearer "+randomValue(t))
				status = 400
			}
			requireStatus(t, run(handler, r), status)
		})
	}
}
