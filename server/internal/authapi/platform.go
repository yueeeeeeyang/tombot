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
	handler.writePlatformPage(w, r, principal, token, r.URL.Path, http.StatusOK, "", false)
}

func (handler *Handler) writePlatformPage(w http.ResponseWriter, r *http.Request, principal identity.Principal, token, path string, status int, message string, isError bool) {
	isAccount, isPassword := path == "/account", path == "/account/password"
	title := "用量管理"
	if isAccount {
		title = "个人信息"
	}
	if isPassword {
		title = "修改密码"
	}
	var page bytes.Buffer
	if err := handler.platform.Execute(&page, struct {
		Title, DisplayName, Username, CSRF, ProfileCSRF, PasswordCSRF, Message string
		IsAccount, IsPassword, IsUsage, IsError                                bool
	}{title, principal.DisplayName, principal.Username, handler.webCSRF("logout", token), handler.webCSRF("profile", token), handler.webCSRF("password", token), message, isAccount, isPassword, !isAccount && !isPassword, isError}); err != nil {
		handler.logger.ErrorContext(r.Context(), "用户平台页面渲染失败", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
		handler.failure(w, r, http.StatusInternalServerError, "PAGE_FAILED", "页面暂时无法显示，请稍后重试")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "strict-origin")
	w.WriteHeader(status)
	if _, err := w.Write(page.Bytes()); err != nil {
		handler.logger.WarnContext(r.Context(), "用户平台响应未发送完成", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
	}
}

// 脚本管理主题及资料编辑交互；身份与资料表单由服务端鉴权并自动转义。
const userPlatformHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>{{.Title}} · TomBot 用户平台</title>
  <link rel="icon" href="/admin/assets/login-logo.png">
  <script src="/admin/assets/user-platform.js"></script>
  {{if .IsAccount}}<script type="module" src="/admin/assets/user-profile.js"></script>{{end}}
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
        <a href="/" aria-label="用量管理" {{if .IsUsage}}aria-current="page"{{end}}>
          {{template "usage-icon"}}<span class="nav-label">用量管理</span>
        </a>
      </nav>
      <div class="navigation-footer">
        <details class="account-control">
          <summary class="account-trigger" aria-label="账户菜单：{{.DisplayName}}（{{.Username}}）">
            <img class="user-avatar" src="/api/v2/web/auth/avatar" alt="" width="36" height="36">
            <span class="account-identity"><strong title="{{.DisplayName}}">{{.DisplayName}}</strong><small title="{{.Username}}">@{{.Username}}</small></span>
            <svg class="account-chevron" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="m6 15 6-6 6 6"/></svg>
          </summary>
          <div class="account-menu">
            <span class="account-menu-label">我的账号</span>
            <a href="/account" {{if .IsAccount}}aria-current="page"{{end}}>个人信息</a>
            <a href="/account/password" {{if .IsPassword}}aria-current="page"{{end}}>修改密码</a>
            <label class="theme-control">主题切换
              <select id="theme-select" aria-label="主题切换">
                <option value="system">跟随系统</option><option value="light">浅色</option><option value="dark">深色</option>
              </select>
            </label>
            <p id="theme-error" role="status" hidden></p>
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
        <p>{{if .IsAccount}}修改你的昵称和头像。{{else if .IsPassword}}为你的账户设置新的登录密码。{{else}}查看和管理你的 TomBot 用量。{{end}}</p>
      </header>
      {{if .Message}}<p class="form-notice {{if .IsError}}form-error{{end}}" role="{{if .IsError}}alert{{else}}status{{end}}">{{.Message}}</p>{{end}}
      {{if .IsAccount}}
      <section class="profile-card" aria-label="账户资料">
        <form id="profile-form" data-username="{{.Username}}" method="post" action="/api/v2/web/auth/profile" enctype="multipart/form-data">
          <input type="hidden" name="csrf" value="{{.ProfileCSRF}}">
          <input id="avatar-source" type="file" accept="image/png,image/jpeg" aria-label="选择头像图片" hidden>
          <div class="profile-heading">
            <button id="avatar-edit" class="avatar-edit" type="button" aria-label="更换头像" aria-describedby="profile-edit-hint" title="更换头像">
              <img id="current-avatar" class="user-avatar" src="/api/v2/web/auth/avatar" alt="当前头像" width="64" height="64">
              <span class="avatar-edit-overlay" aria-hidden="true">更换</span>
            </button>
            <div class="profile-identity">
              <div id="nickname-editor" class="nickname-row">
                <h2 id="nickname-display">{{.DisplayName}}</h2>
                <button id="nickname-edit" class="icon-button" type="button" aria-label="编辑昵称" title="编辑昵称">{{template "pencil-icon"}}</button>
                <input id="nickname-input" name="display_name" value="{{.DisplayName}}" aria-label="昵称" maxlength="120" autocomplete="nickname" aria-describedby="nickname-error" required hidden>
                <button id="nickname-cancel" class="text-button" type="button" hidden>取消</button>
              </div>
              <p>@{{.Username}}</p>
            </div>
          </div>
          <p id="profile-edit-hint" class="field-hint profile-edit-hint">点击头像更换并裁剪；昵称修改后按回车或点击其他位置自动保存。</p>
          <p id="nickname-error" class="field-hint inline-error" role="alert" hidden></p>
          <p id="profile-edit-status" class="field-hint" role="status" hidden></p>
          <dl class="profile-details">
            <div><dt>用户名</dt><dd>{{.Username}}</dd></div>
            <div><dt>账号类型</dt><dd>普通用户</dd></div>
            <div><dt>账号状态</dt><dd>已激活</dd></div>
          </dl>
          <noscript><p class="field-hint">请启用 JavaScript 后编辑头像和昵称。</p></noscript>
        </form>
      </section>
      <dialog id="avatar-crop-dialog" class="crop-dialog" aria-labelledby="crop-title" aria-describedby="crop-hint">
        <h2 id="crop-title">裁剪头像</h2>
        <p id="crop-hint" class="field-hint">拖动图片或使用方向键调整位置，缩放后确认更换。</p>
        <div class="crop-stage">
          <canvas id="crop-canvas" width="512" height="512" tabindex="0" role="img" aria-label="头像裁剪区域" aria-describedby="crop-hint"></canvas>
          <div class="crop-mask" aria-hidden="true"></div>
        </div>
        <label class="crop-zoom">缩放<input id="crop-zoom" type="range" min="1" max="3" step="0.01" value="1" disabled></label>
        <p id="crop-status" class="field-hint" role="status"></p>
        <p id="crop-error" class="form-notice form-error" role="alert" hidden></p>
        <div class="crop-actions">
          <button id="crop-cancel" class="secondary-button" type="button">取消裁剪</button>
          <button id="crop-confirm" class="primary-button" type="button" disabled>确认更换</button>
        </div>
      </dialog>
      {{else if .IsPassword}}
      <section class="profile-card" aria-label="修改密码">
        <form class="account-form" method="post" action="/api/v2/web/auth/password">
          <input type="hidden" name="csrf" value="{{.PasswordCSRF}}">
          <input type="hidden" name="username" value="{{.Username}}" autocomplete="username">
          <label class="field">当前密码<input name="current_password" type="password" autocomplete="current-password" maxlength="1024" required></label>
          <label class="field">新密码<input name="new_password" type="password" autocomplete="new-password" minlength="12" maxlength="1024" aria-describedby="password-hint" required></label>
          <label class="field">确认新密码<input name="confirm_password" type="password" autocomplete="new-password" minlength="12" maxlength="1024" required></label>
          <p id="password-hint" class="field-hint">新密码至少 12 个字符。修改成功后，网页与客户端的所有登录会话都会退出，请使用新密码重新登录。</p>
          <button class="primary-button" type="submit">修改密码</button>
        </form>
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
{{define "pencil-icon"}}<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m16 4 4 4M4 20l4-1 12-12a2.8 2.8 0 0 0-4-4L4 15z"/></svg>{{end}}
{{define "usage-icon"}}<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 3v18h18M8 16v-4m5 4V8m5 8V5"/></svg>{{end}}`
