package authapi

import (
	"errors"
	"net/http"
	"strings"

	"io.yueyang/tom/internal/identity"
)

// Action 在 HTTPS、来源、会话和管理 CSRF 验证之后接收服务端解析的身份与请求标识。
// 具体领域仍须按当前数据库角色和所属关系执行授权，不能只信任界面传来的权限声明。
type Action func(http.ResponseWriter, *http.Request, identity.Principal, identity.Attempt)

// Protect 将相同认证边界应用于业务路由；范围由启动入口固定，路径必须位于对应 v2 命名空间。
func (handler *Handler) Protect(scope string, action Action) (http.Handler, error) {
	if scope != "admin" && scope != "desktop" || action == nil {
		return nil, errors.New("业务认证范围或处理器无效")
	}
	protected := handler.withPrincipal(scope, func(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
		action(w, r, principal, currentAttempt(r))
	})
	return handler.transport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAdmin := strings.HasPrefix(r.URL.Path, "/api/v2/admin/")
		if !strings.HasPrefix(r.URL.Path, "/api/v2/") || isAdmin != (scope == "admin") {
			handler.failure(w, r, http.StatusNotFound, "NOT_FOUND", "接口不存在")
			return
		}
		protected.ServeHTTP(w, r)
	})), nil
}
