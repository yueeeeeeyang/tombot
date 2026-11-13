package serverapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func prepareApplication(t *testing.T, cluster *pgtest.Cluster) (Config, *x509.CertPool, *sql.DB, string) {
	t.Helper()
	configuration := testConfig(t)
	roots := testCertificate(t, configuration, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	for _, path := range []string{configuration.IdentityKeyFile, configuration.CSRFKeyFile, configuration.ModelKeyFiles[configuration.ActiveModelKey]} {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, key)
		clear(key)
	}
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for role, path := range configuration.Database.DSNFiles {
		login := cluster.Login(t, string(role))
		writeTestFile(t, path, []byte(cluster.DSN(name, login)))
	}
	writeNodeClientCertificate(t, configuration.Node)
	if err := os.MkdirAll(filepath.Join(configuration.AdminDirectory, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(configuration.AdminDirectory, "index.html"), []byte(`<html lang="zh-CN"><script src="/admin/assets/app-test.js"></script></html>`))
	writeTestFile(t, filepath.Join(configuration.AdminDirectory, "assets/app-test.js"), []byte(`document.title="机器人管理";`))
	return configuration, roots, pool, name
}

type runningApplication struct {
	done    chan struct{}
	err     error
	cancel  context.CancelFunc
	address string
	client  *http.Client
}

func startApplication(t *testing.T, application *Application, roots *x509.CertPool) *runningApplication {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	running := &runningApplication{done: make(chan struct{}), cancel: cancel, address: listener.Addr().String(), client: &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	go func() { running.err = application.Serve(ctx, listener); close(running.done) }()
	t.Cleanup(func() {
		cancel()
		transport.CloseIdleConnections()
		select {
		case <-running.done:
		case <-time.After(5 * time.Second):
			t.Error("服务测试未退出")
		}
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return running
}

func (running *runningApplication) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-running.done:
		return running.err
	case <-time.After(5 * time.Second):
		t.Fatal("服务未在时限内退出")
		return nil
	}
}

func request(t *testing.T, client *http.Client, method, address, body, origin string) (int, []byte, http.Header) {
	t.Helper()
	r, err := http.NewRequest(method, address, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatal("读取测试响应失败", readErr, closeErr)
	}
	return response.StatusCode, content, response.Header
}

func TestApplicationIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	configuration, roots, pool, name := prepareApplication(t, cluster)
	t.Run("真实组合与退出", func(t *testing.T) {
		application, err := Open(t.Context(), configuration, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := application.Close(); err != nil {
				t.Error(err)
			}
		})
		before := httptest.NewRecorder()
		application.ready(before, httptest.NewRequest("GET", "/health/ready", nil))
		if before.Code != 503 {
			t.Fatal("未启动即就绪")
		}
		running := startApplication(t, application, roots)
		for _, fixture := range []struct {
			method, path, body, origin string
			status                     int
		}{
			{"GET", "/health/live", "", "", 200},
			{"GET", "/health/ready", "", "", 200},
			{"GET", "/admin/", "", "", 200},
			{"GET", "/admin/assets/app-test.js", "", "", 200},
			{"GET", "/admin", "", "", 308},
			{"GET", "/", "", "", 307},
			{"GET", "/admin/missing", "", "", 404},
			{"GET", "/api/v2/auth/session", "", "", 401},
			{"GET", "/api/v2/admin/auth/session", "", "", 401},
			{"GET", "/api/v2/admin/users", "", "", 401},
			{"GET", "/api/v2/bots", "", "", 401},
			{"POST", "/api/v2/auth/login", "{}", "", 400},
			{"POST", "/api/v2/admin/auth/login", "{}", configuration.AdminOrigin, 400},
			{"POST", "/api/v2/admin/auth/login", "{}", "https://foreign.example.test", 403},
			{"GET", "/api/workspace", "", "", 404},
			{"POST", "/api/setup", "{}", "", 404},
		} {
			status, _, headers := request(t, running.client, fixture.method, "https://"+running.address+fixture.path, fixture.body, fixture.origin)
			if status != fixture.status {
				t.Errorf("%s %s: %d，预期 %d", fixture.method, fixture.path, status, fixture.status)
			}
			if fixture.path == "/admin/" && !strings.Contains(headers.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Error("管理页缺少限制头")
			}
		}
		status, _, _ := request(t, running.client, "GET", "http://"+running.address+"/health/live", "", "")
		if status != 400 {
			t.Fatal("明文请求进入业务处理器")
		}
		verifyUserRoutes(t, running, pool, configuration.Node.ID)
		if err := application.Close(); err == nil {
			t.Fatal("在途服务允许关闭数据库")
		}
		if err := application.Serve(t.Context(), nil); err == nil {
			t.Fatal("允许重复启动")
		}
		running.cancel()
		if err := running.wait(t); err != nil {
			t.Fatal("正常取消返回错误", err)
		}
		after := httptest.NewRecorder()
		application.ready(after, httptest.NewRequest("GET", "/health/ready", nil))
		if after.Code != 503 {
			t.Fatal("停止后仍就绪")
		}
		if err := application.Close(); err != nil {
			t.Fatal(err)
		}
		for _, database := range application.databases {
			if err := database.Ping(t.Context()); err == nil {
				t.Fatal("停止后连接仍可使用")
			}
		}
	})
	t.Run("后续用途配置失败释放已开连接", func(t *testing.T) {
		candidate := configuration
		candidate.Database.DSNFiles = map[postgres.Role]string{}
		for role, path := range configuration.Database.DSNFiles {
			candidate.Database.DSNFiles[role] = path
		}
		candidate.Database.DSNFiles[postgres.TenantRole] = filepath.Join(t.TempDir(), "invalid.connection")
		writeTestFile(t, candidate.Database.DSNFiles[postgres.TenantRole], []byte(cluster.DSN(name, "test_owner")))
		application, err := Open(t.Context(), candidate, testLogger())
		if err == nil {
			if closeErr := application.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("接受超级用户连接")
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			var count int
			if err := pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name IN ('tombot_identity','tombot_control','tombot_tenant','tombot_dispatch')`, name).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("启动失败遗留 %d 个连接", count)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("领域配置错误和前端缺失拒绝启动", func(t *testing.T) {
		for _, change := range []func(*Config){func(c *Config) { c.Computer.MaxWaiting = 0 }, func(c *Config) { c.Models.Origins = nil }, func(c *Config) { c.Node.ID = "invalid" }, func(c *Config) { c.AdminDirectory = filepath.Join(t.TempDir(), "absent") }} {
			candidate := configuration
			change(&candidate)
			application, err := Open(t.Context(), candidate, testLogger())
			if err == nil {
				if closeErr := application.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("接受缺失或错误依赖")
			}
		}
	})
	t.Run("数据库失联使就绪检查失败", func(t *testing.T) {
		application, err := Open(t.Context(), configuration, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := application.Close(); err != nil {
				t.Error(err)
			}
		})
		// 单独检查探针，避免工作者同时处理人为关闭的连接。
		application.state.Store(stateRunning)
		if err := application.databases[postgres.TenantRole].Close(); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		application.ready(recorder, httptest.NewRequest("GET", "/health/ready", nil))
		application.state.Store(stateStopped)
		if recorder.Code != 503 || !strings.Contains(recorder.Body.String(), `"scope":"api"`) {
			t.Fatal("数据库失联仍宣布就绪")
		}
	})
}

func verifyUserRoutes(t *testing.T, running *runningApplication, pool *sql.DB, nodeID string) {
	t.Helper()
	execute := func(query string, arguments ...any) {
		t.Helper()
		if _, err := pool.Exec(query, arguments...); err != nil {
			t.Fatal("准备组合测试记录失败", err)
		}
	}
	call := func(method, path string, body any, token identity.Credential, target any) int {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal("测试请求无法编码")
		}
		r, err := http.NewRequest(method, "https://"+running.address+path, strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal("测试请求无法构造")
		}
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+string(token))
		}
		response, err := running.client.Do(r)
		if err != nil {
			t.Fatal("测试 HTTPS 请求失败")
		}
		content, readErr := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal("读取测试响应失败")
		}
		if target != nil && response.StatusCode < 300 && json.Unmarshal(content, target) != nil {
			t.Fatal("响应无法解码")
		}
		return response.StatusCode
	}
	randomValue := func() string {
		t.Helper()
		value, err := secure.Token()
		if err != nil {
			t.Fatal("生成测试凭据失败")
		}
		return value
	}
	image := ids.New()
	// 本测试只准备电脑目录，不将数据库状态视为真实 Docker 能力验收。
	execute(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, "example.test/computer@sha256:"+secure.Hash(image))
	execute(`INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',10,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, nodeID, image)
	sessions := make([]identity.Session, 2)
	for index := range sessions {
		user, computer, activation, password := ids.New(), ids.New(), randomValue(), randomValue()
		execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'组合测试用户','provisioning')`, user, strings.ToLower(user))
		execute(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit) VALUES($1,$2,$3,$4,'ready',$2,$2,1000,1073741824,1073741824,128)`, user, computer, nodeID, image)
		execute(`UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, user)
		execute(`INSERT INTO tombot.activation_tokens(id,user_id,token_digest,expires_at) VALUES($1,$2,decode($3,'hex'),clock_timestamp()+interval '1 hour')`, ids.New(), user, secure.Hash(activation))
		if status := call("POST", "/api/v2/auth/activate", map[string]string{"activation_token": activation, "password": password}, "", nil); status != 204 {
			t.Fatal("组合入口无法激活账户", status)
		}
		if status := call("POST", "/api/v2/auth/login", map[string]string{"username": strings.ToLower(user), "password": password, "device_name": "组合测试客户端", "platform": "linux"}, "", &sessions[index]); status != 200 || sessions[index].Principal.UserID != user {
			t.Fatal("组合入口返回错误登录身份", status)
		}
	}
	var created struct {
		Bot bots.Bot `json:"bot"`
	}
	create := bots.CreateRequest{ClientRequestID: ids.New(), Spec: bots.Spec{Name: "测试机器人", SystemPrompt: "You are a helpful bot.", ApprovalMode: "standard", ToolPermissions: []string{"computer_shell"}}}
	if status := call("POST", "/api/v2/bots", create, sessions[0].AccessToken, &created); status != 201 || created.Bot.DesktopID == "" {
		t.Fatal("组合入口无法创建独立桌面身份", status)
	}
	for _, fixture := range []struct {
		token  identity.Credential
		path   string
		status int
	}{
		{sessions[0].AccessToken, "/api/v2/bots/" + created.Bot.ID, 200},
		{sessions[1].AccessToken, "/api/v2/bots/" + created.Bot.ID, 404},
		{sessions[0].AccessToken, "/api/v2/admin/users", 400},
	} {
		if status := call("GET", fixture.path, nil, fixture.token, nil); status != fixture.status {
			t.Fatal("组合入口所属权或管理范围错误", fixture.path, status)
		}
	}
	if status := call("POST", "/api/v2/auth/logout", map[string]any{}, sessions[0].AccessToken, nil); status != 204 {
		t.Fatal("组合入口无法退出", status)
	}
	if status := call("GET", "/api/v2/bots", nil, sessions[0].AccessToken, nil); status != 401 {
		t.Fatal("退出后仍可读取用户数据", status)
	}
	if status := call("GET", "/api/v2/bots", nil, sessions[1].AccessToken, nil); status != 200 {
		t.Fatal("退出影响其他用户", status)
	}
}

func TestServeLifecycle(t *testing.T) {
	for _, reason := range []string{"工作者错误", "工作者无故退出", "外部取消", "取消与工作者退出竞争"} {
		t.Run(reason, func(t *testing.T) {
			configuration := testConfig(t)
			roots := testCertificate(t, configuration, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
			inbound, err := loadTLS(configuration)
			if err != nil {
				t.Fatal(err)
			}
			trigger := make(chan struct{})
			exited := make(chan struct{})
			sentinel := errors.New("工作者测试错误")
			application := &Application{configuration: configuration, tls: inbound, logger: testLogger(), handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), background: []func(context.Context) error{
				func(ctx context.Context) error {
					if reason == "外部取消" || reason == "取消与工作者退出竞争" {
						<-ctx.Done()
						return ctx.Err()
					}
					<-trigger
					if reason == "工作者错误" {
						return sentinel
					}
					return nil
				},
				func(ctx context.Context) error { defer close(exited); <-ctx.Done(); return ctx.Err() },
			}}
			running := startApplication(t, application, roots)
			request(t, running.client, "GET", "https://"+running.address, "", "")
			if reason == "外部取消" || reason == "取消与工作者退出竞争" {
				running.cancel()
			} else {
				close(trigger)
			}
			err = running.wait(t)
			if reason == "工作者错误" && !errors.Is(err, sentinel) {
				t.Fatal("未保留工作者错误", err)
			}
			if reason == "工作者无故退出" && err == nil {
				t.Fatal("工作者提前退出被掩盖")
			}
			if (reason == "外部取消" || reason == "取消与工作者退出竞争") && err != nil {
				t.Fatal("取消时出现伪故障", err)
			}
			select {
			case <-exited:
			default:
				t.Fatal("服务退出早于工作者回收")
			}
			if err := application.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShutdownTimeoutRetainsResources(t *testing.T) {
	configuration := testConfig(t)
	configuration.HTTP.ShutdownTimeout = Duration(100 * time.Millisecond)
	roots := testCertificate(t, configuration, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	inbound, err := loadTLS(configuration)
	if err != nil {
		t.Fatal(err)
	}
	release, exited := make(chan struct{}), make(chan struct{})
	application := &Application{configuration: configuration, tls: inbound, logger: testLogger(), handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), background: []func(context.Context) error{func(context.Context) error { defer close(exited); <-release; return nil }}}
	t.Cleanup(func() {
		close(release)
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Error("测试工作者未释放")
		}
	})
	running := startApplication(t, application, roots)
	request(t, running.client, "GET", "https://"+running.address, "", "")
	running.cancel()
	if err := running.wait(t); err == nil || !strings.Contains(err.Error(), "退出超时") {
		t.Fatal("退出超时未报告", err)
	}
	if application.state.Load() != stateStopping || application.Close() == nil {
		t.Fatal("仍有在途工作却释放资源")
	}
}

// writeNodeClientCertificate 为节点客户端生成一次性证书。
// 服务启动期不连接节点，因此这里只需要可解析的证书与私钥文件，不建立真实连接。
func writeNodeClientCertificate(t *testing.T, configuration NodeConfig) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "tombot-api-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, configuration.CertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeTestFile(t, configuration.PrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	clear(private)
	clear(encoded)
}
