//go:build browserintegration

package deployment

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServerHTTPSBrowserHandshake 使用真实 Chromium 复现协议错误；只信任本次临时证书的公钥。
func TestServerHTTPSBrowserHandshake(t *testing.T) {
	module := os.Getenv("TOM_TEST_PLAYWRIGHT_MODULE")
	if module == "" {
		t.Fatal("需要 TOM_TEST_PLAYWRIGHT_MODULE 指定项目 Playwright 模块")
	}
	if executable := os.Getenv("TOM_TEST_BROWSER_BINARY"); executable != "" && !filepath.IsAbs(executable) {
		t.Fatal("TOM_TEST_BROWSER_BINARY 必须为浏览器可执行文件绝对路径")
	}
	for _, legacy := range []bool{true, false} {
		name := "浏览器兼容证书"
		if legacy {
			name = "旧证书协议不匹配"
		}
		t.Run(name, func(t *testing.T) {
			certificate, key, err := createServerCertificate("127.0.0.1", false, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				clear(key)
				certificate, key = legacyHTTPSCertificate(t, false)
			}
			defer clear(key)
			pair, err := tls.X509KeyPair(certificate, key)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			fingerprint := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/html; charset=utf-8")
				if _, err := io.WriteString(writer, "<!doctype html><title>TomBot HTTPS</title><p>HTTPS 已连接</p>"); err != nil {
					t.Error(err)
				}
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
			server.StartTLS()
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			script := `
const { chromium } = await import(process.argv[1]);
const browser = await chromium.launch({ executablePath: process.env.TOM_TEST_BROWSER_BINARY || undefined, args: ['--ignore-certificate-errors-spki-list=' + process.argv[3]] });
try {
  const page = await browser.newPage();
  const response = await page.goto(process.argv[2], { waitUntil: 'domcontentloaded', timeout: 15000 });
  if (response.status() !== 200 || await page.title() !== 'TomBot HTTPS') throw new Error('HTTPS 页面验证失败');
} finally {
  await browser.close();
}
`
			command := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, module, server.URL, base64.StdEncoding.EncodeToString(fingerprint[:]))
			output, err := command.CombinedOutput()
			if legacy {
				if err == nil || !strings.Contains(string(output), "ERR_SSL_VERSION_OR_CIPHER_MISMATCH") {
					t.Fatalf("未复现浏览器协议错误：%v\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("浏览器仍不能访问新证书：%v\n%s", err, output)
			}
		})
	}
}
