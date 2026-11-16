package authapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"io.yueyang/tom/internal/identity"
)

// webProfile 限制整个上传体积和字段数量，头像内容的可信校验交给身份领域。
func (handler *Handler) webProfile(w http.ResponseWriter, r *http.Request) {
	principal, token, err := handler.webPrincipal(r)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	fail := func(status int, message string) {
		handler.failure(w, r, status, "PROFILE_INVALID", message)
	}
	r.Body = http.MaxBytesReader(w, r.Body, identity.MaxUserAvatarBytes+maxBodyBytes)
	err = r.ParseMultipartForm(identity.MaxUserAvatarBytes + maxBodyBytes)
	if r.MultipartForm != nil {
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				handler.logger.ErrorContext(r.Context(), "头像上传临时文件清理失败", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
			}
		}()
	}
	if err != nil {
		fail(http.StatusBadRequest, "上传内容无效或过大，请使用不超过 2 MiB 的图片。")
		return
	}
	for name, values := range r.PostForm {
		if len(values) != 1 || (name != "csrf" && name != "display_name") {
			fail(http.StatusBadRequest, "资料表单无效，请刷新后重试。")
			return
		}
	}
	for name, files := range r.MultipartForm.File {
		if name != "avatar" || len(files) != 1 || len(r.PostForm["avatar"]) != 0 {
			fail(http.StatusBadRequest, "请选择一张头像图片。")
			return
		}
	}
	if !handler.checkWebCSRF(w, r, "profile", token) {
		return
	}
	var avatar []byte
	if files := r.MultipartForm.File["avatar"]; len(files) == 1 {
		file, err := files[0].Open()
		if err != nil {
			fail(http.StatusBadRequest, "无法读取上传图片，请重新选择。")
			return
		}
		avatar, err = io.ReadAll(io.LimitReader(file, identity.MaxUserAvatarBytes+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			fail(http.StatusBadRequest, "无法读取上传图片，请重新选择。")
			return
		}
	}
	update := identity.ProfileUpdate{Avatar: avatar}
	if values, exists := r.PostForm["display_name"]; exists {
		update.DisplayName = &values[0]
	}
	profile, err := handler.identity.UpdateProfile(r.Context(), principal, update, currentAttempt(r).RequestID)
	if errors.Is(err, identity.ErrInvalidInput) {
		fail(http.StatusBadRequest, "昵称须为 1–120 个字符，不能包含控制字符。")
		return
	}
	if errors.Is(err, identity.ErrInvalidAvatar) {
		fail(http.StatusBadRequest, identity.ErrInvalidAvatar.Error())
		return
	}
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, http.StatusOK, profile)
}

// webAvatar 的资源地址不接收用户 ID，避免通过路径或查询参数读取其他用户头像。
func (handler *Handler) webAvatar(w http.ResponseWriter, r *http.Request) {
	principal, _, err := handler.webPrincipal(r)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	content, err := handler.identity.UserAvatar(r.Context(), principal)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	if content == nil {
		http.Redirect(w, r, "/admin/assets/login-logo.png", http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if _, err := w.Write(content); err != nil {
		handler.logger.WarnContext(r.Context(), "用户头像响应未发送完成", "request_id", currentAttempt(r).RequestID, "user_id", principal.UserID)
	}
}

// webPassword 复用身份领域的口令验证和全端撤销规则，失败响应不回填任何口令。
func (handler *Handler) webPassword(w http.ResponseWriter, r *http.Request) {
	if !handler.parseWebForm(w, r) {
		return
	}
	principal, token, err := handler.webPrincipal(r)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	if !handler.checkWebCSRF(w, r, "password", token) {
		return
	}
	fail := func(status int, message string) {
		handler.writePlatformPage(w, r, principal, token, "/account/password", status, message, true)
	}
	if r.PostForm.Get("new_password") != r.PostForm.Get("confirm_password") {
		fail(http.StatusBadRequest, "两次输入的新密码不一致。")
		return
	}
	err = handler.identity.ChangePassword(r.Context(), principal, r.PostForm.Get("current_password"), r.PostForm.Get("new_password"), "", currentAttempt(r))
	switch {
	case errors.Is(err, identity.ErrInvalidCredentials):
		fail(http.StatusUnauthorized, "当前密码不正确，请重新输入。")
		return
	case errors.Is(err, identity.ErrInvalidInput):
		fail(http.StatusBadRequest, "新密码至少 12 个字符，且不超过 1024 字节。")
		return
	case errors.Is(err, identity.ErrRateLimited):
		fail(http.StatusTooManyRequests, "尝试次数过多，请稍后再试。")
		return
	case err != nil:
		handler.serviceError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: webSessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	http.Redirect(w, r, "/login?password=changed", http.StatusSeeOther)
}
