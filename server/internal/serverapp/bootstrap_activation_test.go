package serverapp

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
)

// verifyBootstrapActivation 将部署签发的真实材料交给正式 HTTPS 入口，核对激活与管理 Cookie 链路。
func verifyBootstrapActivation(t *testing.T, configuration Config, roots *x509.CertPool, activation accounts.Activation, username string) {
	t.Helper()
	application, err := Open(t.Context(), configuration, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(); err != nil {
			t.Error(err)
		}
	})
	running := startApplication(t, application, roots)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	running.client.Jar = jar
	call := func(path string, body any) (int, []byte, http.Header) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal("管理员激活测试请求编码失败")
		}
		return request(t, running.client, "POST", "https://"+running.address+path, string(encoded), configuration.AdminOrigin)
	}
	token := map[string]string{"activation_token": string(activation.Value)}
	status, content, headers := call("/api/v2/auth/admin-enrollment", token)
	if status != 200 || headers.Get("Cache-Control") != "no-store" || len(headers.Values("Set-Cookie")) != 0 {
		t.Fatal("管理员登记响应边界错误", status)
	}
	var enrollment struct {
		Secret    identity.Credential `json:"secret"`
		Period    int                 `json:"period"`
		Digits    int                 `json:"digits"`
		Algorithm string              `json:"algorithm"`
	}
	if json.Unmarshal(content, &enrollment) != nil || enrollment.Period != 30 || enrollment.Digits != 6 || enrollment.Algorithm != "SHA1" {
		t.Fatal("管理员登记协议错误")
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(string(enrollment.Secret))
	if err != nil || len(secret) != 20 {
		t.Fatal("管理员登记材料长度错误")
	}
	defer clear(secret)
	status, repeated, _ := call("/api/v2/auth/admin-enrollment", token)
	if status != 200 || string(content) != string(repeated) {
		t.Fatal("重复登记改变了验证器材料")
	}
	password, err := secure.Token()
	if err != nil {
		t.Fatal(err)
	}
	activate := map[string]string{"activation_token": string(activation.Value), "password": password, "code": ""}
	if status, _, _ := call("/api/v2/auth/activate", activate); status != 401 {
		t.Fatal("未验证动态码也能激活管理员", status)
	}
	// 激活用当前步，登录用允许窗口内的下一步，避免跨时间步边界造成测试误报。
	step := time.Now().Unix() / 30
	activate["code"] = bootstrapTOTP(t, secret, step)
	status, content, headers = call("/api/v2/auth/activate", activate)
	if status != 204 || len(content) != 0 || len(headers.Values("Set-Cookie")) != 0 {
		t.Fatal("管理员激活失败或隐式签发会话", status)
	}
	if status, _, _ := call("/api/v2/auth/activate", activate); status != 400 {
		t.Fatal("已消费的激活材料可以重放", status)
	}
	if status, _, _ := call("/api/v2/auth/admin-enrollment", token); status != 400 {
		t.Fatal("已激活账户仍可读取登记材料", status)
	}
	login := map[string]string{"username": username, "password": password, "code": activate["code"], "device_name": "管理员激活联合测试"}
	if status, _, _ := call("/api/v2/admin/auth/login", login); status != 401 {
		t.Fatal("登录接受了激活时已使用的动态码", status)
	}
	login["code"] = bootstrapTOTP(t, secret, max(step+1, time.Now().Unix()/30))
	status, content, headers = call("/api/v2/admin/auth/login", login)
	if status != 200 {
		t.Fatal("激活后管理员不能登录", status)
	}
	var session struct {
		Principal identity.Principal `json:"principal"`
	}
	if json.Unmarshal(content, &session) != nil || session.Principal.UserID != activation.UserID || session.Principal.Role != "admin" || session.Principal.Scope != "admin" {
		t.Fatal("激活后登录身份不符")
	}
	if strings.Contains(string(content), "access_token") || strings.Contains(string(content), "refresh_token") {
		t.Fatal("管理登录响应向页面暴露会话值")
	}
	cookies := (&http.Response{Header: headers}).Cookies()
	if len(cookies) != 2 {
		t.Fatal("管理员登录缺少会话 Cookie")
	}
	for _, cookie := range cookies {
		if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
			t.Fatal("管理员 Cookie 边界无效")
		}
	}
	status, _, _ = request(t, running.client, "GET", "https://"+running.address+"/api/v2/admin/users", "", "")
	if status != 200 {
		t.Fatal("新管理员会话无法访问管理接口", status)
	}
	running.cancel()
	if err := running.wait(t); err != nil {
		t.Fatal("管理员联合测试服务未正常退出", err)
	}
}

func bootstrapTOTP(t *testing.T, secret []byte, step int64) string {
	t.Helper()
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	if _, err := mac.Write(counter[:]); err != nil {
		t.Fatal("管理员测试动态码计算失败")
	}
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 15
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}
