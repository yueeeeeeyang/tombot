package authapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
)

func TestDesktopProfileRequiresCurrentUser(t *testing.T) {
	handler, stub, _ := fixture(t)
	stub.session.Principal.Username = "profile-owner"
	stub.session.Principal.DisplayName = "本人昵称"
	requireStatus(t, run(handler, request("GET", "/api/v2/auth/profile", "")), http.StatusUnauthorized)
	r := request("GET", "/api/v2/auth/profile?user_id=untrusted", "")
	r.Header.Set("Authorization", "Bearer "+string(stub.session.AccessToken))
	for _, content := range [][]byte{nil, []byte("PNG")} {
		stub.avatar = content
		result := run(handler, r)
		requireStatus(t, result, http.StatusOK)
		var response struct {
			Profile struct {
				Username, Avatar string
				DisplayName      string `json:"display_name"`
			} `json:"profile"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
			t.Fatal("资料响应无效", err)
		}
		expected := ""
		if len(content) > 0 {
			expected = "data:image/png;base64," + base64.StdEncoding.EncodeToString(content)
		}
		if response.Profile.Username != "profile-owner" || response.Profile.DisplayName != "本人昵称" || response.Profile.Avatar != expected || stub.principal.UserID != stub.session.Principal.UserID || stub.principal.Scope != "desktop" || result.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("资料未绑定当前用户或缺少缓存限制")
		}
		if strings.Contains(result.Body.String(), string(stub.session.AccessToken)) {
			t.Fatal("资料响应泄露凭据")
		}
	}
	stub.profileErr = errors.New("测试数据库故障")
	requireStatus(t, run(handler, r), http.StatusServiceUnavailable)
}

func TestWebProfileForm(t *testing.T) {
	for _, mode := range []string{"valid", "nickname-only", "avatar-only", "duplicate", "wrong-csrf", "foreign-csrf", "invalid-name", "invalid-image", "too-large", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			if mode == "avatar-only" {
				stub.displayName = "当前昵称"
			}
			token := string(stub.session.AccessToken)
			csrf := handler.webCSRF("profile", token)
			if mode == "wrong-csrf" {
				csrf = handler.webCSRF("logout", token)
			}
			if mode == "foreign-csrf" {
				csrf = handler.webCSRF("profile", randomValue(t))
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for key, value := range map[string]string{"csrf": csrf, "display_name": "新昵称"} {
				if mode == "avatar-only" && key == "display_name" {
					continue
				}
				if err := writer.WriteField(key, value); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "duplicate" {
				if err := writer.WriteField("display_name", "重复"); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "nickname-only" {
				part, err := writer.CreateFormFile("avatar", "avatar.png")
				if err != nil {
					t.Fatal(err)
				}
				content := []byte("由领域验证图像")
				if mode == "too-large" {
					content = make([]byte, identity.MaxUserAvatarBytes+maxBodyBytes)
				}
				if _, err := part.Write(content); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "invalid-name":
				stub.profileErr = identity.ErrInvalidInput
			case "invalid-image":
				stub.profileErr = identity.ErrInvalidAvatar
			case "unavailable":
				stub.profileErr = errors.New("测试数据库不可用")
			}
			r := request("POST", "/api/v2/web/auth/profile", body.String())
			r.Header.Set("Content-Type", writer.FormDataContentType())
			r.Header.Set("Origin", "https://"+r.Host)
			r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: token})
			result := run(handler, r)
			expected := 200
			switch mode {
			case "duplicate", "invalid-name", "invalid-image", "too-large":
				expected = 400
			case "wrong-csrf", "foreign-csrf":
				expected = 403
			case "unavailable":
				expected = 503
			}
			requireStatus(t, result, expected)
			if expected == 200 {
				var profile identity.UserProfile
				expectedName := "新昵称"
				if mode == "avatar-only" {
					expectedName = "当前昵称"
				}
				if json.Unmarshal(result.Body.Bytes(), &profile) != nil || profile.DisplayName != expectedName || stub.displayName != expectedName || stub.principal.UserID != stub.session.Principal.UserID || result.Header().Get("Location") != "" {
					t.Fatal("资料更新未返回当前身份的 JSON 结果")
				}
			}
			if mode == "nickname-only" && stub.avatar != nil {
				t.Fatal("未选择文件仍覆盖头像")
			}
			if mode == "duplicate" || mode == "wrong-csrf" || mode == "foreign-csrf" || mode == "too-large" {
				for _, call := range stub.calls {
					if call == "profile" {
						t.Fatal("拒绝的表单执行了更新")
					}
				}
			}
		})
	}
}

func TestWebPasswordForm(t *testing.T) {
	for _, mode := range []string{"valid", "mismatch", "wrong-csrf", "duplicate", "invalid-current", "short", "limited", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			handler, stub, _ := fixture(t)
			token := string(stub.session.AccessToken)
			current, next := randomValue(t), randomValue(t)
			form := url.Values{"csrf": {handler.webCSRF("password", token)}, "current_password": {current}, "new_password": {next}, "confirm_password": {next}}
			expected := 303
			switch mode {
			case "mismatch":
				form.Set("confirm_password", randomValue(t))
				expected = 400
			case "wrong-csrf":
				form.Set("csrf", handler.webCSRF("profile", token))
				expected = 403
			case "duplicate":
				form.Add("new_password", next)
				expected = 400
			case "invalid-current":
				stub.passwordErr = identity.ErrInvalidCredentials
				expected = 401
			case "short":
				stub.passwordErr = identity.ErrInvalidInput
				expected = 400
			case "limited":
				stub.passwordErr = identity.ErrRateLimited
				expected = 429
			case "unavailable":
				stub.passwordErr = errors.New("测试不可用")
				expected = 503
			}
			r := request("POST", "/api/v2/web/auth/password", form.Encode())
			r.Header.Set("Origin", "https://"+r.Host)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: token})
			result := run(handler, r)
			requireStatus(t, result, expected)
			if strings.Contains(result.Body.String(), current) || strings.Contains(result.Body.String(), next) {
				t.Fatal("错误页面回填了密码")
			}
			if mode == "valid" {
				if result.Header().Get("Location") != "/login?password=changed" {
					t.Fatal("改密未要求重新登录")
				}
				cookies := result.Result().Cookies()
				if len(cookies) != 1 || cookies[0].MaxAge != -1 {
					t.Fatal("改密未清理会话 Cookie")
				}
			}
			if mode == "mismatch" || mode == "wrong-csrf" || mode == "duplicate" {
				for _, call := range stub.calls {
					if call == "password" {
						t.Fatal("无效表单执行了改密")
					}
				}
			}
		})
	}
}

func TestWebAvatarRequiresCurrentUser(t *testing.T) {
	handler, stub, _ := fixture(t)
	requireStatus(t, run(handler, request("GET", "/api/v2/web/auth/avatar", "")), 401)
	r := request("GET", "/api/v2/web/auth/avatar?user_id=untrusted", "")
	r.AddCookie(&http.Cookie{Name: webSessionCookie, Value: string(stub.session.AccessToken)})
	result := run(handler, r)
	requireStatus(t, result, 307)
	if result.Header().Get("Location") != "/admin/assets/login-logo.png" {
		t.Fatal("默认头像不存在")
	}
	stub.avatar = []byte("PNG")
	result = run(handler, r)
	requireStatus(t, result, 200)
	if result.Body.String() != "PNG" || result.Header().Get("Content-Type") != "image/png" || result.Header().Get("Cache-Control") != "no-store" || stub.principal.UserID != stub.session.Principal.UserID {
		t.Fatal("头像未使用当前身份或被缓存")
	}
	stub.profileErr = errors.New("测试数据库故障")
	requireStatus(t, run(handler, r), 503)
}
