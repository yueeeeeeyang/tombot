package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sassoftware/relic/v8/lib/certloader"
	"software.sslmate.com/src/go-pkcs12"
)

// 创建同名但不同密钥的发布者，以验证匹配的是证书而非显示名称。
func testPublisher(t *testing.T) *certloader.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TomBot Windows Updates"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &certloader.Certificate{Leaf: leaf, PrivateKey: key}
}

func TestVerifyUpdate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unsigned := filepath.Join(dir, "unsigned.exe")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", unsigned, source)
	build.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建测试 PE 失败：%v，%s", err, output)
	}
	sign := func(name string, cert *certloader.Certificate) string {
		t.Helper()
		original, err := os.ReadFile(unsigned)
		if err != nil {
			t.Fatal(err)
		}
		password := rand.Text()
		identity := filepath.Join(dir, name+".pfx")
		pfx, err := pkcs12.Modern.Encode(cert.PrivateKey, cert.Leaf, nil, password)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(identity, pfx, 0600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, name)
		if err := os.WriteFile(target, original, 0600); err != nil {
			t.Fatal(err)
		}
		run := func(pass string) error {
			cmd := exec.CommandContext(ctx, "go", "run", "-mod=readonly", "./cmd/sign", target, identity)
			cmd.Env = append(os.Environ(), "WIN_CSC_KEY_PASSWORD="+pass)
			return cmd.Run()
		}
		if err := run(rand.Text()); err == nil {
			t.Fatal("错误口令被接受")
		}
		if err := run(password); err != nil {
			t.Fatal(err)
		}
		return target
	}
	signer := testPublisher(t)
	installed := sign("TomBot.exe", signer)
	update := sign("update.exe", signer)
	if err := verifyUpdate(installed, update); err != nil {
		t.Fatalf("同发布者的签名升级失败：%v", err)
	}
	for _, invalid := range []string{sign("foreign.exe", testPublisher(t)), unsigned} {
		if err := verifyUpdate(installed, invalid); err == nil {
			t.Fatalf("错误接受了 %s", filepath.Base(invalid))
		}
	}
	contents, err := os.ReadFile(update)
	if err != nil {
		t.Fatal(err)
	}
	contents[0x40] ^= 1
	if err := os.WriteFile(update, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpdate(installed, update); err == nil {
		t.Fatal("错误接受了已篡改的可执行文件")
	}
}
