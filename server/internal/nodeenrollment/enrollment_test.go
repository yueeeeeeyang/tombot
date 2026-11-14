package nodeenrollment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func certificate(t *testing.T) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "节点测试"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}
func signed(t *testing.T, pair tls.Certificate, claim Claim) Request {
	t.Helper()
	body, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	return Request{claim, base64.RawURLEncoding.EncodeToString(ed25519.Sign(pair.PrivateKey.(ed25519.PrivateKey), body))}
}
func claimFor(pair tls.Certificate) Claim {
	id := ids.New()
	return Claim{NodeID: id, Registration: nodecatalog.Registration{Name: "节点" + id, Capacity: 2, ImageID: ids.New(), ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64)}, URL: "https://" + strings.ToLower(id) + ".example.test:9443", Certificate: pair.Certificate[0], IssuedAt: time.Now().Unix()}
}

type authorizer struct {
	failure error
	recent  bool
}

func (a *authorizer) RequireAdmin(_ context.Context, _ identity.Principal, recent bool) error {
	a.recent = recent
	return a.failure
}

func serviceFixture(t *testing.T, maximum int) (*Service, *sql.DB, identity.Principal, *authorizer) {
	t.Helper()
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(t.Context(), postgres.Options{DSN: cluster.DSN(name, cluster.Login(t, string(postgres.ControlRole))), Role: postgres.ControlRole, MaxConnections: 8, ConnectTimeout: time.Second * 5, StatementTimeout: time.Second * 5, LockTimeout: time.Second * 3, TransactionTimeout: time.Second * 10, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	actor := identity.Principal{UserID: ids.New()}
	if _, err := pool.Exec(`INSERT INTO tombot.administrators(id,username,display_name,status) VALUES($1,'enrollment-admin','接入管理员','active')`, actor.UserID); err != nil {
		t.Fatal(err)
	}
	auth := &authorizer{}
	service, err := New(db, auth, maximum)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool, actor, auth
}

func TestEnrollmentLifecycle(t *testing.T) {
	service, pool, actor, auth := serviceFixture(t, 2)
	grant, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil || !auth.recent {
		t.Fatal("签发失败或未要求近期验证", err)
	}
	pair := certificate(t)
	claim := claimFor(pair)
	request := signed(t, pair, claim)
	if _, err := service.Register(t.Context(), request, "", ids.New()); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("无凭据首次接入被接受", err)
	}
	// 并发重试仅消费一次，回执丢失后可凭已绑定签名恢复。
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Register(t.Context(), request, grant.Token, ids.New()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := service.Register(t.Context(), request, "", ids.New()); err != nil {
		t.Fatal("重启未复用身份", err)
	}
	var count int
	if err := pool.QueryRow(`SELECT count(*) FROM tombot.node_connections`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var hash []byte
	if err := pool.QueryRow(`SELECT token_hash FROM tombot.node_enrollment_tokens WHERE id=$1`, grant.ID).Scan(&hash); err != nil || len(hash) != 32 || bytes.Contains(hash, []byte(grant.Token)) {
		t.Fatal("凭据摘要错误", err)
	}
	otherPair := certificate(t)
	other := claimFor(otherPair)
	if _, err := service.Register(t.Context(), signed(t, otherPair, other), grant.Token, ids.New()); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("凭据复用被接受", err)
	}
	grant2, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(t.Context(), signed(t, otherPair, other), grant2.Token, ids.New()); err != nil {
		t.Fatal("同镜像第二节点接入失败", err)
	}
	endpoints, err := service.List(t.Context(), "", 1)
	if err != nil || len(endpoints) != 1 {
		t.Fatal(endpoints, err)
	}
	rest, err := service.List(t.Context(), endpoints[0].NodeID, 1)
	if err != nil || len(rest) != 1 || rest[0].NodeID == endpoints[0].NodeID {
		t.Fatal("分页重复或遗漏", err)
	}
	endpoint, err := service.Resolve(t.Context(), other.NodeID)
	if err != nil || endpoint.URL != other.URL {
		t.Fatal("解析到错误节点", err)
	}
	claim.URL = "https://changed.example.test"
	if _, err := service.Register(t.Context(), signed(t, pair, claim), "", ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("重启覆盖原连接", err)
	}
	claim.URL = request.Claim.URL
	claim.Certificate = otherPair.Certificate[0]
	if _, err := service.Register(t.Context(), signed(t, otherPair, claim), "", ids.New()); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("另一证书冒充节点", err)
	}
	grant3, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	third := certificate(t)
	if _, err := service.Register(t.Context(), signed(t, third, claimFor(third)), grant3.Token, ids.New()); !errors.Is(err, ErrCapacity) {
		t.Fatal("突破节点上限", err)
	}
	var consumed sql.NullString
	if err := pool.QueryRow(`SELECT consumed_node_id FROM tombot.node_enrollment_tokens WHERE id=$1`, grant3.ID).Scan(&consumed); err != nil || consumed.Valid {
		t.Fatal("失败消耗了凭据", err)
	}
	auth.failure = identity.ErrVerificationRequired
	if _, err := service.Issue(t.Context(), actor, ids.New()); !errors.Is(err, identity.ErrVerificationRequired) {
		t.Fatal("跳过近期验证", err)
	}
}

func TestEnrollmentRollbackAndExpiry(t *testing.T) {
	service, pool, actor, _ := serviceFixture(t, 8)
	grant, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE tombot.node_enrollment_tokens SET created_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 day' WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	pair := certificate(t)
	request := signed(t, pair, claimFor(pair))
	if _, err := service.Register(t.Context(), request, grant.Token, ids.New()); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("接受过期凭据", err)
	}
	grant, err = service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`REVOKE INSERT ON tombot.node_connections FROM tombot_control`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(t.Context(), request, grant.Token, ids.New()); err == nil || strings.Contains(err.Error(), grant.Token) {
		t.Fatal("事务失败未报告或泄漏凭据")
	}
	var nodes int
	var consumed sql.NullString
	if err := pool.QueryRow(`SELECT count(*) FROM tombot.computer_nodes`).Scan(&nodes); err != nil || nodes != 0 {
		t.Fatal("部分目录未回滚", err)
	}
	if err := pool.QueryRow(`SELECT consumed_node_id FROM tombot.node_enrollment_tokens WHERE id=$1`, grant.ID).Scan(&consumed); err != nil || consumed.Valid {
		t.Fatal("失败消耗凭据", err)
	}
}

func TestClaimVerification(t *testing.T) {
	pair := certificate(t)
	claim := claimFor(pair)
	if _, err := signed(t, pair, claim).Verify(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Claim){func(c *Claim) { c.IssuedAt -= 3600 }, func(c *Claim) { c.IssuedAt += 3600 }, func(c *Claim) { c.URL = "http://node.example.test" }, func(c *Claim) { c.NodeID = "invalid" }, func(c *Claim) { c.Registration.Capacity = 0 }, func(c *Claim) { c.Certificate = []byte("invalid") }} {
		changed := claim
		change(&changed)
		if _, err := signed(t, pair, changed).Verify(time.Now()); err == nil {
			t.Fatal("非法登记被接受")
		}
	}
	request := signed(t, pair, claim)
	request.Claim.Registration.Name = "被替换"
	if _, err := request.Verify(time.Now()); err == nil {
		t.Fatal("篡改签名被接受")
	}
}

type registrarFunc func(context.Context, Request, string, string) (Receipt, error)

func (f registrarFunc) Register(c context.Context, r Request, token, id string) (Receipt, error) {
	return f(c, r, token, id)
}

func TestClientEnrollmentAndRestart(t *testing.T) {
	pair := certificate(t)
	claim := claimFor(pair)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	registered := false
	requests := 0
	handler, err := NewHandler(registrarFunc(func(_ context.Context, r Request, received, _ string) (Receipt, error) {
		requests++
		if _, err := r.Verify(time.Now()); err != nil {
			return Receipt{}, err
		}
		if !registered && received != token {
			return Receipt{}, ErrUnauthorized
		}
		registered = true
		return Receipt{claim.NodeID, claim.Registration.ImageID, "registered"}, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate(t)}}
	server.StartTLS()
	defer server.Close()
	pin, err := nodeapi.FingerprintOf(server.TLS.Certificates[0])
	if err != nil {
		t.Fatal(err)
	}
	configuration := Config{Server: ServerConfig{URL: server.URL, NodeURL: claim.URL, Fingerprints: []string{pin}, TokenFile: "/private/node-enrollment.token"}, Registration: claim.Registration}
	reads := 0
	read := func(string) ([]byte, error) { reads++; return []byte(token + "\n"), nil }
	if err := Register(t.Context(), claim.NodeID, configuration, pair, read); err != nil {
		t.Fatal(err)
	}
	if err := Register(t.Context(), claim.NodeID, configuration, pair, func(string) ([]byte, error) {
		t.Error("重启读取一次性凭据")
		return nil, errors.New("不可读")
	}); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || requests != 3 {
		t.Fatal("认证挑战或重启次数不正确", reads, requests)
	}
}

func TestEnrollmentHTTPBounds(t *testing.T) {
	var log bytes.Buffer
	calls := 0
	handler, err := NewHandler(registrarFunc(func(context.Context, Request, string, string) (Receipt, error) {
		calls++
		return Receipt{}, ErrUnauthorized
	}), slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"unexpected":true}`, strings.Repeat("x", maximumBodyBytes+1), `{} {}`} {
		request := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || calls != 0 {
			t.Fatal("非法正文到达服务", response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || calls != 1 {
		t.Fatal(response.Code, calls)
	}
}

type unavailableRuntime struct {
	computer.Runtime
	calls sync.Map
}

func (runtime *unavailableRuntime) Heartbeat(_ context.Context, query computer.NodeQuery) (computer.NodeHeartbeat, error) {
	runtime.calls.Store(query.NodeID, true)
	return computer.NodeHeartbeat{}, errors.New("节点暂不可达")
}

func TestFleetDiscoversAdditionalNodesAndStops(t *testing.T) {
	service, _, actor, _ := serviceFixture(t, 3)
	enroll := func() string {
		t.Helper()
		grant, err := service.Issue(t.Context(), actor, ids.New())
		if err != nil {
			t.Fatal(err)
		}
		pair := certificate(t)
		claim := claimFor(pair)
		if _, err := service.Register(t.Context(), signed(t, pair, claim), grant.Token, ids.New()); err != nil {
			t.Fatal(err)
		}
		return claim.NodeID
	}
	first, second := enroll(), enroll()
	runtime := &unavailableRuntime{}
	fleet, err := NewFleet(service, service.control, runtime, nodecatalog.ObserverOptions{CallTimeout: time.Second, Interval: time.Second, Lifetime: time.Minute}, nodecatalog.VerificationOptions{Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 128}, CallTimeout: time.Second, LeaseDuration: 10 * time.Second, Interval: time.Minute, Lifetime: 2 * time.Minute, RetryDelay: time.Second, MaxAttempts: 3}, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fleet.Run(ctx) }()
	wait := func(id string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := runtime.calls.Load(id); ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("节点未得到独立心跳监督", id)
	}
	wait(first)
	wait(second)
	third := enroll()
	wait(third)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("节点监督取消后未释放")
	}
}

func TestClientRejectsRedirectAndWrongPin(t *testing.T) {
	pair := certificate(t)
	claim := claimFor(pair)
	calls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	pin, err := nodeapi.FingerprintOf(server.TLS.Certificates[0])
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Server: ServerConfig{URL: server.URL, NodeURL: claim.URL, Fingerprints: []string{pin}, TokenFile: "/private/enrollment.token"}, Registration: claim.Registration}
	if err := Register(t.Context(), claim.NodeID, config, pair, func(string) ([]byte, error) { t.Error("重定向不应读取凭据"); return nil, ErrUnauthorized }); err == nil || calls != 0 {
		t.Fatal("登记跟随重定向", err)
	}
	config.Server.Fingerprints = []string{strings.Repeat("a", 64)}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := Register(ctx, claim.NodeID, config, pair, func(string) ([]byte, error) {
		t.Error("不可信服务不应获取凭据")
		return nil, ErrUnauthorized
	}); err == nil {
		t.Fatal("错误服务端证书仍接入")
	}
}
