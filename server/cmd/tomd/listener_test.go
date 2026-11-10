package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/config"
)

func TestServeWithAndWithoutTLS(t *testing.T) {
	cfg := config.Config{TLSCertFile: filepath.Join(t.TempDir(), "chain.pem"), TLSKeyFile: filepath.Join(t.TempDir(), "private.pem")}
	if _, err := loadServerTLS(cfg); err == nil {
		t.Fatal("缺失证书时仍可启动")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(cfg.TLSCertFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.TLSKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	trust := x509.NewCertPool()
	if !trust.AppendCertsFromPEM(certPEM) {
		t.Fatal("测试根不可读")
	}
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			settings := config.Config{}
			if scheme == "https" {
				settings = cfg
			}
			inbound, err := loadServerTLS(settings)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{TLSConfig: inbound, ReadHeaderTimeout: time.Second, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
			result := make(chan error, 1)
			go func() { result <- serveListener(server, listener) }()
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := server.Shutdown(ctx); err != nil {
					t.Error(err)
				}
				if err := <-result; !errors.Is(err, http.ErrServerClosed) {
					t.Error(err)
				}
			}()
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}}
			client := http.Client{Transport: transport, Timeout: 3 * time.Second}
			defer transport.CloseIdleConnections()
			call := func(protocol string) int {
				response, err := client.Get(protocol + "://" + listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				if err = response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				return response.StatusCode
			}
			if call(scheme) != http.StatusNoContent {
				t.Fatal("请求未到达业务处理器")
			}
			if scheme == "https" && call("http") != http.StatusBadRequest {
				t.Fatal("TLS 端口接受明文业务请求")
			}
		})
	}
	if err = os.WriteFile(cfg.TLSKeyFile, []byte("损坏"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadServerTLS(cfg); err == nil {
		t.Fatal("私钥损坏时降级监听")
	}
}
