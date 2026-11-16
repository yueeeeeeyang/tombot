package authapi

import (
	"bytes"
	"errors"
	"net/http"

	"io.yueyang/tom/internal/identity"
)

// platformPage 只渲染当前网页会话的资料；每次导航重新鉴权，不在浏览器缓存用户信息。
func (handler *Handler) platformPage(w http.ResponseWriter, r *http.Request) {
	principal, token, err := handler.webPrincipal(r)
	if errors.Is(err, identity.ErrInvalidSession) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	isAccount := r.URL.Path == "/account"
	title := "用量管理"
	if isAccount {
		title = "个人信息"
	}
	var page bytes.Buffer
	if err := handler.platform.Execute(&page, struct {
		Title, DisplayName, Username, CSRF string
		IsAccount                          bool
	}{title, principal.DisplayName, principal.Username, handler.webCSRF("logout", token), isAccount}); err != nil {
		handler.logger.ErrorContext(r.Context(), "用户平台页面渲染失败", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
		handler.failure(w, r, http.StatusInternalServerError, "PAGE_FAILED", "页面暂时无法显示，请稍后重试")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "strict-origin")
	if _, err := w.Write(page.Bytes()); err != nil {
		handler.logger.WarnContext(r.Context(), "用户平台响应未发送完成", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
	}
}

// 页面不用脚本读取身份或保存凭据，模板自动转义账户名称及表单校验值。
const userPlatformHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>{{.Title}} · TomBot 用户平台</title>
  <link rel="icon" href="/admin/assets/login-logo.png">
  <link rel="stylesheet" href="/admin/assets/user-platform.css">
</head>
<body>
  <a class="skip-link" href="#main">跳转到主要内容</a>
  <div class="user-shell">
    <aside class="navigation" aria-label="用户侧栏">
      <a class="brand" href="/" aria-label="TomBot 用户平台首页">
        <img src="/admin/assets/login-logo.png" alt="" width="40" height="40">
        <span class="brand-title">TomBot<small>用户平台</small></span>
      </a>
      <nav aria-label="用户导航">
        <a href="/" aria-label="用量管理" {{if not .IsAccount}}aria-current="page"{{end}}>
          {{template "usage-icon"}}<span class="nav-label">用量管理</span>
        </a>
      </nav>
      <div class="navigation-footer">
        <details class="account-control">
          <summary class="account-trigger" aria-label="账户菜单：{{.DisplayName}}（{{.Username}}）">
            <img src="/admin/assets/login-logo.png" alt="" width="36" height="36">
            <span class="account-identity"><strong title="{{.DisplayName}}">{{.DisplayName}}</strong><small title="{{.Username}}">@{{.Username}}</small></span>
            <svg class="account-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="m6 15 6-6 6 6"/></svg>
          </summary>
          <div class="account-menu">
            <span class="account-menu-label">我的账号</span>
            <a href="/account" {{if .IsAccount}}aria-current="page"{{end}}>个人信息</a>
            <form method="post" action="/api/v2/web/auth/logout">
              <input type="hidden" name="csrf" value="{{.CSRF}}">
              <button type="submit">退出登录</button>
            </form>
          </div>
        </details>
      </div>
    </aside>
    <main id="main" class="page-content" tabindex="-1">
      <header class="page-heading">
        <h1>{{.Title}}</h1>
        <p>{{if .IsAccount}}查看你的账户资料。{{else}}查看和管理你的 TomBot 用量。{{end}}</p>
      </header>
      {{if .IsAccount}}
      <section class="profile-card" aria-label="账户资料">
        <div class="profile-heading">
          <img src="/admin/assets/login-logo.png" alt="" width="56" height="56">
          <div><h2>{{.DisplayName}}</h2><p>@{{.Username}}</p></div>
        </div>
        <dl class="profile-details">
          <div><dt>显示名称</dt><dd>{{.DisplayName}}</dd></div>
          <div><dt>用户名</dt><dd>{{.Username}}</dd></div>
          <div><dt>账号类型</dt><dd>普通用户</dd></div>
          <div><dt>账号状态</dt><dd>已激活</dd></div>
        </dl>
        <p class="account-hint">在桌面客户端发起登录后，可直接使用此账号授权。</p>
      </section>
      {{else}}
      <section class="empty-state" aria-labelledby="usage-placeholder-title">
        <div class="empty-icon">{{template "usage-icon"}}</div>
        <h2 id="usage-placeholder-title">用量管理即将上线</h2>
        <p>未来将在这里查看用量明细与统计。</p>
      </section>
      {{end}}
    </main>
  </div>
</body>
</html>
{{define "usage-icon"}}<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 3v18h18M8 16v-4m5 4V8m5 8V5"/></svg>{{end}}`
