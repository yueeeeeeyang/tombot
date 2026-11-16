package serverapp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
	"io.yueyang/tom/web/admin/bundle"
)

type browserLoginFixture struct {
	running                             *runningApplication
	pool                                *sql.DB
	username, password, certificateFile string
}

func loginTestToken(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal("测试随机材料生成失败")
	}
	return value
}

// newBrowserLoginFixture 使用完整应用、真实数据库与临时浏览器兼容证书，覆盖实际路由组合。
func newBrowserLoginFixture(t *testing.T) browserLoginFixture {
	t.Helper()
	cluster := pgtest.Start(t)
	configuration, _, pool, _ := prepareApplication(t, cluster)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "浏览器登录测试"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: false, SignatureAlgorithm: x509.ECDSAWithSHA256}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
	writeTestFile(t, configuration.TLSCertificateFile, encoded)
	writeTestFile(t, configuration.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(encoded) {
		t.Fatal("测试证书不可读取")
	}
	page, err := bundle.New()
	if err != nil {
		t.Fatal(err)
	}
	application, err := Open(t.Context(), configuration, page, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	fixture := browserLoginFixture{running: startApplication(t, application, roots), pool: pool, username: strings.ToLower(ids.New()), password: loginTestToken(t), certificateFile: configuration.TLSCertificateFile}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.running.client.Jar = jar
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(query, args...); err != nil {
			t.Fatal("准备浏览器登录记录失败", err)
		}
	}
	user, node, image, computer := ids.New(), ids.New(), ids.New(), ids.New()
	activation := loginTestToken(t)
	execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'浏览器登录用户','provisioning')`, user, fixture.username)
	execute(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/computer@sha256:"+secure.Hash(image))
	execute(`INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,$1,'bootstrapping',1)`, node)
	execute(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, user, computer, node, image)
	execute(`UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, user)
	execute(`INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at) VALUES($1,$2,decode($3,'hex'),clock_timestamp()+interval '1 hour')`, ids.New(), user, secure.Hash(activation))
	status, _, _ := fixture.call(t, "POST", "/api/v2/auth/activate", map[string]string{"activation_token": activation, "password": fixture.password}, "")
	if status != 204 {
		t.Fatal("测试用户无法激活", status)
	}
	return fixture
}

// call 隐藏认证正文，失败时只报告状态；查询用同一个已核验证书的客户端。
func (fixture browserLoginFixture) call(t *testing.T, method, path string, body any, access identity.Credential) (int, []byte, http.Header) {
	t.Helper()
	contentType := "application/json"
	var encoded []byte
	var err error
	if form, ok := body.(url.Values); ok {
		encoded = []byte(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	} else if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal("请求无法编码")
		}
	}
	request, err := http.NewRequest(method, "https://"+fixture.running.address+path, strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal("请求无法构建")
	}
	request.Header.Set("Content-Type", contentType)
	if contentType == "application/x-www-form-urlencoded" {
		request.Header.Set("Origin", "https://"+fixture.running.address)
	}
	if access != "" {
		request.Header.Set("Authorization", "Bearer "+string(access))
	}
	response, err := fixture.running.client.Do(request)
	if err != nil {
		t.Fatal("登录联合请求失败")
	}
	result, readErr := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal("登录响应读取失败")
	}
	return response.StatusCode, result, response.Header
}

func TestBrowserLoginThroughApplication(t *testing.T) {
	fixture := newBrowserLoginFixture(t)
	verifier, state := loginTestToken(t), loginTestToken(t)
	challenge := sha256.Sum256([]byte(verifier))
	status, content, _ := fixture.call(t, "POST", "/api/v2/auth/browser/start", map[string]string{"redirect_uri": "http://127.0.0.1:49152/callback", "state": state, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "device_name": "浏览器联合测试", "platform": "linux"}, "")
	if status != 200 {
		t.Fatal("未登录不能发起浏览器授权", status)
	}
	var started struct {
		URL string `json:"authorization_url"`
	}
	if json.Unmarshal(content, &started) != nil {
		t.Fatal("授权响应格式无效")
	}
	page, err := url.Parse(started.URL)
	if err != nil || page.Path != "/authorize" || page.Host != "" {
		t.Fatal("授权页地址无效")
	}
	status, content, _ = fixture.call(t, "GET", started.URL, nil, "")
	if status != 200 || !strings.Contains(string(content), `name="password"`) {
		t.Fatal("真实组合路由未提供登录页", status)
	}
	form := browserForm(t, content)
	form.Set("username", fixture.username)
	form.Set("password", loginTestToken(t))
	status, _, _ = fixture.call(t, "POST", "/api/v2/auth/browser/approve", form, "")
	if status != 401 {
		t.Fatal("错误口令被接受", status)
	}
	form.Set("password", fixture.password)
	status, _, headers := fixture.call(t, "POST", "/api/v2/auth/browser/approve", form, "")
	if status != 303 {
		t.Fatal("正确口令不能批准授权", status)
	}
	callback, err := url.Parse(headers.Get("Location"))
	if err != nil || callback.Query().Get("state") != state || callback.Query().Get("code") == "" {
		t.Fatal("授权回调无效")
	}
	exchange := map[string]string{"code": callback.Query().Get("code"), "state": state, "code_verifier": loginTestToken(t)}
	status, _, _ = fixture.call(t, "POST", "/api/v2/auth/browser/exchange", exchange, "")
	if status != 400 {
		t.Fatal("错误 PKCE 校验串未拒绝", status)
	}
	exchange["code_verifier"] = verifier
	exchange["state"] = loginTestToken(t)
	status, _, _ = fixture.call(t, "POST", "/api/v2/auth/browser/exchange", exchange, "")
	if status != 400 {
		t.Fatal("错误状态未拒绝", status)
	}
	exchange["state"] = state
	status, content, _ = fixture.call(t, "POST", "/api/v2/auth/browser/exchange", exchange, "")
	if status != 200 {
		t.Fatal("有效 S256 挑战无法换取会话", status)
	}
	var session identity.Session
	if json.Unmarshal(content, &session) != nil || session.AccessToken == "" || session.RefreshToken == "" || session.Principal.Username != fixture.username {
		t.Fatal("授权未产生正确用户会话")
	}
	status, _, _ = fixture.call(t, "GET", "/api/v2/auth/session", nil, session.AccessToken)
	if status != 200 {
		t.Fatal("换取的会话不可使用", status)
	}
	status, _, _ = fixture.call(t, "POST", "/api/v2/auth/browser/exchange", exchange, "")
	if status != 400 {
		t.Fatal("授权码允许重复换取会话", status)
	}
	status, _, _ = fixture.call(t, "GET", started.URL, nil, "")
	if status != 410 {
		t.Fatal("已消费的登录链接仍可使用", status)
	}
}

// browserFormForAction 避免多表单页面把其他操作的 CSRF 用于退出登录。
func browserFormForAction(t *testing.T, content []byte, action string) url.Values {
	t.Helper()
	form := regexp.MustCompile(`(?s)<form[^>]*action="` + regexp.QuoteMeta(action) + `"[^>]*>.*?</form>`).Find(content)
	if form == nil {
		t.Fatal("页面缺少指定操作的表单")
	}
	return browserForm(t, form)
}

// browserForm 只解析服务端生成的隐藏字段，断言不输出认证材料。
func browserForm(t *testing.T, content []byte) url.Values {
	t.Helper()
	fields := url.Values{}
	for _, match := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllStringSubmatch(string(content), -1) {
		fields.Set(html.UnescapeString(match[1]), html.UnescapeString(match[2]))
	}
	if fields.Get("csrf") == "" {
		t.Fatal("页面缺少请求校验")
	}
	return fields
}

func (fixture browserLoginFixture) startAuthorization(t *testing.T) (string, string, string) {
	t.Helper()
	verifier, state := loginTestToken(t), loginTestToken(t)
	challenge := sha256.Sum256([]byte(verifier))
	status, content, _ := fixture.call(t, "POST", "/api/v2/auth/browser/start", map[string]string{"redirect_uri": "http://127.0.0.1:49152/callback", "state": state, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "device_name": "我的桌面", "platform": "linux"}, "")
	var started struct {
		URL string `json:"authorization_url"`
	}
	if status != 200 || json.Unmarshal(content, &started) != nil || started.URL == "" {
		t.Fatal("发起授权失败", status)
	}
	return started.URL, verifier, state
}

func TestUserWebLoginAndDesktopAuthorization(t *testing.T) {
	fixture := newBrowserLoginFixture(t)
	for _, path := range []string{"/", "/account"} {
		status, _, headers := fixture.call(t, "GET", path, nil, "")
		if status != 303 || headers.Get("Location") != "/login" {
			t.Fatal("未登录可进入用户平台", path, status)
		}
	}
	status, content, _ := fixture.call(t, "GET", "/login", nil, "")
	if status != 200 {
		t.Fatal("普通登录入口不可用", status)
	}
	form := browserForm(t, content)
	form.Set("username", fixture.username)
	form.Set("password", fixture.password)
	status, content, headers := fixture.call(t, "POST", "/api/v2/web/auth/login", form, "")
	if status != 303 || headers.Get("Location") != "/" {
		t.Fatal("网页登录失败", status)
	}
	var webToken string
	for _, c := range (&http.Response{Header: headers}).Cookies() {
		if c.Name == "__Host-tombot-user" {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
				t.Fatal("会话 Cookie 限制不完整")
			}
			webToken = c.Value
		}
	}
	if webToken == "" || strings.Contains(string(content), webToken) {
		t.Fatal("网页会话未隔离于页面正文")
	}
	for _, path := range []string{"/api/v2/auth/session", "/api/v2/admin/auth/session"} {
		status, _, _ = fixture.call(t, "GET", path, nil, identity.Credential(webToken))
		if status < 400 {
			t.Fatal("网页凭据越过会话范围", path)
		}
	}
	status, content, _ = fixture.call(t, "GET", "/", nil, "")
	if status != 200 || !strings.Contains(string(content), "用量管理即将上线") || !strings.Contains(string(content), fixture.username) || !strings.Contains(string(content), `class="navigation-footer"`) || strings.Contains(string(content), webToken) {
		t.Fatal("用户平台未显示当前用户与占位内容", status)
	}
	status, content, _ = fixture.call(t, "GET", "/account", nil, "")
	if status != 200 || !strings.Contains(string(content), fixture.username) || !strings.Contains(string(content), "个人信息") || strings.Contains(string(content), webToken) {
		t.Fatal("个人信息不正确", status)
	}
	logout := browserFormForAction(t, content, "/api/v2/web/auth/logout")
	status, _, headers = fixture.call(t, "GET", "/login", nil, "")
	if status != 303 || headers.Get("Location") != "/" {
		t.Fatal("已登录仍索要密码")
	}
	target, verifier, state := fixture.startAuthorization(t)
	status, content, _ = fixture.call(t, "GET", target, nil, "")
	if status != 200 || strings.Contains(string(content), `name="password"`) || !strings.Contains(string(content), "授权客户端") {
		t.Fatal("已有网页会话不能直接授权", status)
	}
	approve := browserForm(t, content)
	invalid := browserForm(t, content)
	invalid.Set("csrf", loginTestToken(t))
	status, _, _ = fixture.call(t, "POST", "/api/v2/auth/browser/approve", invalid, "")
	if status != 403 {
		t.Fatal("错误授权校验被接受", status)
	}
	status, _, headers = fixture.call(t, "POST", "/api/v2/auth/browser/approve", approve, "")
	if status != 303 {
		t.Fatal("复用网页会话授权失败", status)
	}
	callback, err := url.Parse(headers.Get("Location"))
	if err != nil {
		t.Fatal("回调无效")
	}
	status, content, _ = fixture.call(t, "POST", "/api/v2/auth/browser/exchange", map[string]string{"code": callback.Query().Get("code"), "code_verifier": verifier, "state": state}, "")
	var desktop identity.Session
	if status != 200 || json.Unmarshal(content, &desktop) != nil || desktop.Principal.Username != fixture.username || desktop.Principal.Scope != "desktop" {
		t.Fatal("复用授权未建立正确桌面会话", status)
	}
	target, _, _ = fixture.startAuthorization(t)
	_, content, _ = fixture.call(t, "GET", target, nil, "")
	stale := browserForm(t, content)
	status, _, _ = fixture.call(t, "POST", "/api/v2/web/auth/logout", logout, "")
	if status != 303 {
		t.Fatal("网页退出失败", status)
	}
	status, content, _ = fixture.call(t, "POST", "/api/v2/auth/browser/approve", stale, "")
	if status != 401 || !strings.Contains(string(content), `name="password"`) {
		t.Fatal("退出后仍可授权", status)
	}
	// 恢复旧 Cookie 模拟另一个标签页持有旧材料，服务端也必须拒绝。
	base, _ := url.Parse("https://" + fixture.running.address)
	fixture.running.client.Jar.SetCookies(base, []*http.Cookie{{Name: "__Host-tombot-user", Value: webToken, Path: "/", Secure: true}})
	status, _, headers = fixture.call(t, "GET", "/account", nil, "")
	if status != 303 || headers.Get("Location") != "/login" {
		t.Fatal("退出未撤销服务端会话", status)
	}
	status, _, _ = fixture.call(t, "GET", "/api/v2/auth/session", nil, desktop.AccessToken)
	if status != 200 {
		t.Fatal("网页退出错误撤销了桌面会话", status)
	}
}

func TestUserWebSessionInvalidation(t *testing.T) {
	fixture := newBrowserLoginFixture(t)
	for _, reason := range []string{"expired", "password-changed", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			_, content, _ := fixture.call(t, "GET", "/login", nil, "")
			form := browserForm(t, content)
			form.Set("username", fixture.username)
			form.Set("password", fixture.password)
			status, _, _ := fixture.call(t, "POST", "/api/v2/web/auth/login", form, "")
			if status != 303 {
				t.Fatal("准备网页会话失败", status)
			}
			target, _, _ := fixture.startAuthorization(t)
			_, content, _ = fixture.call(t, "GET", target, nil, "")
			approve := browserForm(t, content)
			query := "UPDATE tombot.user_sessions SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE scope='web'"
			if reason == "password-changed" {
				query = "UPDATE tombot.users SET auth_version=auth_version+1 WHERE username=$1"
			}
			if reason == "disabled" {
				query = "UPDATE tombot.users SET status='disabled' WHERE username=$1"
			}
			var err error
			if reason == "expired" {
				_, err = fixture.pool.Exec(query)
			} else {
				_, err = fixture.pool.Exec(query, fixture.username)
			}
			if err != nil {
				t.Fatal("准备失效状态失败", err)
			}
			status, content, _ = fixture.call(t, "POST", "/api/v2/auth/browser/approve", approve, "")
			if status != 401 || !strings.Contains(string(content), `name="password"`) {
				t.Fatal("失效会话仍允许批准", status)
			}
			if reason == "disabled" {
				if _, err = fixture.pool.Exec("UPDATE tombot.users SET status='active' WHERE username=$1", fixture.username); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
