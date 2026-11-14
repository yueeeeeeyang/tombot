package nodeenrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
)

func bootstrapConfiguration() BootstrapConfiguration {
	return BootstrapConfiguration{ClientFingerprints: []string{strings.Repeat("a", 64)}, VerificationResources: computer.Resources{CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 1 << 30, ProcessLimit: 512}}
}
func TestBootstrapCodeAndAuthorization(t *testing.T) {
	service, pool, actor, _ := serviceFixture(t, 4)
	var bootstrap *Bootstrap
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bootstrap.ServeHTTP(w, r) }))
	defer server.Close()
	pin, err := nodeapi.FingerprintOf(tls.Certificate{Certificate: [][]byte{server.Certificate().Raw}})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err = NewBootstrap(service, server.URL, []string{pin}, bootstrapConfiguration(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := bootstrap.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	code, err := ParseJoinCode([]byte(grant.Token))
	if err != nil || code.URL != server.URL || code.Fingerprints[0] != pin {
		t.Fatal("接入码未携带正确首次信任", err)
	}
	for range 2 {
		configuration, err := FetchBootstrap(t.Context(), code)
		if err != nil || configuration.ClientFingerprints[0] != bootstrapConfiguration().ClientFingerprints[0] {
			t.Fatal("获取配置失败或内容不符", err)
		}
	}
	var consumed bool
	if err := pool.QueryRow(`SELECT consumed_node_id IS NOT NULL FROM tombot.node_enrollment_tokens WHERE id=$1`, grant.ID).Scan(&consumed); err != nil || consumed {
		t.Fatal("只读获取配置消费了凭据", err)
	}
	pair := certificate(t)
	claim := claimFor(pair)
	if _, err := service.Register(t.Context(), signed(t, pair, claim), code.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchBootstrap(t.Context(), code); err == nil {
		t.Fatal("已消费凭据仍能获取配置")
	}
	for _, invalid := range []string{code.Token, grant.Token + "=", strings.Repeat("x", MaximumJoinCode+1), joinPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"url":"https://a.test","url":"https://b.test"}`))} {
		if _, err := ParseJoinCode([]byte(invalid)); err == nil {
			t.Fatal("接受裸凭据或歧义接入码")
		}
	}
	second, err := bootstrap.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	secondCode, err := ParseJoinCode([]byte(second.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE tombot.node_enrollment_tokens SET created_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 day' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchBootstrap(t.Context(), secondCode); err == nil {
		t.Fatal("过期凭据仍提供配置")
	}
}
func TestBootstrapDoesNotSendTokenBeforeTrustAndRejectsBadResponses(t *testing.T) {
	service, _, actor, _ := serviceFixture(t, 2)
	grant, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	body := []byte(strings.Repeat("x", maximumBodyBytes+1))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if _, err := w.Write(body); err != nil {
			t.Error("响应写入失败")
		}
	}))
	defer server.Close()
	server.Config.ErrorLog = nil
	code := JoinCode{URL: server.URL, Fingerprints: []string{strings.Repeat("b", 64)}, Token: grant.Token}
	if _, err := FetchBootstrap(t.Context(), code); err == nil || calls.Load() != 0 {
		t.Fatal("证书未验证就发送接入请求")
	}
	pin, err := nodeapi.FingerprintOf(tls.Certificate{Certificate: [][]byte{server.Certificate().Raw}})
	if err != nil {
		t.Fatal(err)
	}
	code.Fingerprints = []string{pin}
	if _, err := FetchBootstrap(t.Context(), code); err == nil {
		t.Fatal("接受超限响应")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := calls.Load()
	if _, err := FetchBootstrap(ctx, code); err == nil || calls.Load() != before {
		t.Fatal("取消仍发送请求")
	}
	if bytes.Contains(body, []byte(grant.Token)) {
		t.Fatal("响应夹具包含接入凭据")
	}
}
func TestBootstrapRejectsBrowserAndRedirect(t *testing.T) {
	service, _, _, _ := serviceFixture(t, 2)
	bootstrap, err := NewBootstrap(service, "https://api.example.test", []string{strings.Repeat("a", 64)}, bootstrapConfiguration(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"method", "query", "origin", "authorization"} {
		r := httptest.NewRequest(http.MethodGet, BootstrapPath, nil)
		switch kind {
		case "method":
			r.Method = http.MethodPost
		case "query":
			r.URL.RawQuery = "token=forbidden"
		case "origin":
			r.Header.Set("Origin", "https://api.example.test")
		case "authorization":
			r.Header.Set("Authorization", "invalid")
		}
		w := httptest.NewRecorder()
		bootstrap.ServeHTTP(w, r)
		if w.Code == 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("非法请求通过或缺少防缓存")
		}
	}
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example.test", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	pin, err := nodeapi.FingerprintOf(tls.Certificate{Certificate: [][]byte{server.Certificate().Raw}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := FetchBootstrap(ctx, JoinCode{URL: server.URL, Fingerprints: []string{pin}, Token: base64.RawURLEncoding.EncodeToString(raw)}); err == nil {
		t.Fatal("重定向被接受")
	}
}
