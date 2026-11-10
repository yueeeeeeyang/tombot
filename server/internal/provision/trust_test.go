package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestVerifyAnchorRequiresTrustedCA(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, public, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	runner := &trustProbe{reject: true}
	if verifyMacAnchors(t.Context(), runner, encoded) == nil {
		t.Fatal("未受信根证书被接受")
	}
	runner.reject = false
	if err = verifyMacAnchors(t.Context(), runner, encoded); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatal("未使用系统验证器")
	}
	if _, err = os.Stat(runner.file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("临时公开证书未清理")
	}
	if verifyMacAnchors(t.Context(), runner, []byte("损坏")) == nil {
		t.Fatal("损坏证书列表被接受")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = verifyMacAnchors(ctx, runner, encoded); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

}

func TestMacNativeTrustStore(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("需要 macOS 原生信任库")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := checkSystemRoots(ctx, Environment{OS: runtime.GOOS, Runner: CommandRunner{}}); err != nil {
		t.Fatal(err)
	}
}

type trustProbe struct {
	CommandRunner
	reject bool
	calls  int
	file   string
}

func (r *trustProbe) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls++
	r.file = args[len(args)-1]
	der, err := os.ReadFile(r.file)
	if err != nil {
		return nil, err
	}
	if _, err = x509.ParseCertificate(der); err != nil {
		return nil, err
	}
	if r.reject {
		return nil, errors.New("系统拒绝此证书")
	}
	return nil, nil
}
