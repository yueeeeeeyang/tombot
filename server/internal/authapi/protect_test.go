package authapi

import (
	"net/http"
	"testing"

	"io.yueyang/tom/internal/identity"
)

func TestProtectBusinessRoutes(t *testing.T) {
	handler, stub, _ := fixture(t)
	calls := 0
	protected, err := handler.Protect("admin", func(w http.ResponseWriter, r *http.Request, principal identity.Principal, attempt identity.Attempt) {
		calls++
		if principal.UserID != stub.session.Principal.UserID || attempt.RequestID == "" || !attempt.Source.IsValid() {
			t.Error("未传递可信身份")
		}
		w.WriteHeader(204)
	})
	if err != nil {
		t.Fatal(err)
	}
	r := adminRequest(handler, stub, "POST", "/../business", `{}`)
	r.URL.Path = "/api/v2/admin/users"
	requireStatus(t, run(protected, r), 204)
	if calls != 1 {
		t.Fatal("授权业务操作未执行")
	}
	r.Header.Del("X-CSRF-Token")
	requireStatus(t, run(protected, r), 403)
	if calls != 1 {
		t.Fatal("业务路由绕过 CSRF")
	}
	r = adminRequest(handler, stub, "POST", "/unused", `{}`)
	r.URL.Path = "/api/v2/users"
	requireStatus(t, run(protected, r), 404)
	if calls != 1 {
		t.Fatal("管理处理器可挂接桌面命名空间")
	}
	if _, err = handler.Protect("user", func(http.ResponseWriter, *http.Request, identity.Principal, identity.Attempt) {}); err == nil {
		t.Fatal("未知认证范围被接受")
	}
	if _, err = handler.Protect("admin", nil); err == nil {
		t.Fatal("空业务处理器被接受")
	}
}
