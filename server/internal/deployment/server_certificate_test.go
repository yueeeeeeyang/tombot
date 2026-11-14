package deployment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/serverapp"
)

// legacyHTTPSCertificate 复现旧安装器的 Ed25519 服务身份；机构签发分支用于验证重新签发边界。
func legacyHTTPSCertificate(t *testing.T, caSigned bool) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "192.0.2.20"}, IPAddresses: []net.IP{net.ParseIP("192.0.2.20")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	parent, signer := leaf, private
	if caSigned {
		_, signer, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(signer)
		parent = &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "测试证书机构"}, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	}
	certificate, err := x509.CreateCertificate(rand.Reader, leaf, parent, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func installLegacyHTTPSFixture(t *testing.T, installer serverInstaller, caSigned bool) serverapp.Config {
	t.Helper()
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := legacyHTTPSCertificate(t, caSigned)
	defer clear(key)
	writeInstallFixture(t, installer.fs, configuration.TLSCertificateFile, certificate, 0644)
	writeInstallFixture(t, installer.fs, configuration.TLSPrivateKeyFile, key, 0600)
	return configuration
}

func TestServerCertificateSupportsBrowserSignatureSchemes(t *testing.T) {
	clientHello := &tls.ClientHelloInfo{ServerName: "192.0.2.20", SupportedVersions: []uint16{tls.VersionTLS13}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256, tls.PKCS1WithSHA256, tls.ECDSAWithP384AndSHA384}}
	oldCertificate, oldKey := legacyHTTPSCertificate(t, false)
	defer clear(oldKey)
	oldPair, err := tls.X509KeyPair(oldCertificate, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if clientHello.SupportsCertificate(&oldPair) == nil {
		t.Fatal("未复现浏览器拒绝旧服务证书")
	}
	certificate, key, err := createServerCertificate("192.0.2.20", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientHello.SupportsCertificate(&pair); err != nil {
		t.Fatal("新证书仍不能完成浏览器签名协商", err)
	}
}

func TestServerHealthRejectsBrowserIncompatibleCertificate(t *testing.T) {
	installer, _ := serverInstallFixture(t)
	installServerFixture(t, installer)
	configuration := installLegacyHTTPSFixture(t, installer, false)
	if err := installer.checkServerHealth(t.Context(), configuration); err == nil || !strings.Contains(err.Error(), "浏览器不兼容") {
		t.Fatal("健康检查没有识别浏览器证书兼容问题", err)
	}
}

func TestServerUpgradeRenewsHTTPSAndResumesWithoutRotatingAgain(t *testing.T) {
	installer, host := serverInstallFixture(t)
	installServerFixture(t, installer)
	original := installLegacyHTTPSFixture(t, installer, false)
	preserved := map[string][]byte{}
	for _, path := range []string{original.IdentityKeyFile, original.CSRFKeyFile, original.Node.CertificateFile, original.Node.PrivateKeyFile, original.Database.MigrationDSNFile, original.TLSCertificateFile, original.TLSPrivateKeyFile} {
		preserved[path] = readServerFixture(t, installer, path)
	}
	candidate := readServerFixture(t, installer, serverBinaryPath)
	candidate[len(candidate)-1] = 7
	writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
	host.failStart = true
	report, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{RenewHTTPS: true}, "/source/new-tomd")
	if err == nil || report.Backup == "" {
		t.Fatal("模拟启动失败未保留备份", err)
	}
	api, err := installer.account(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := installer.loadServerConfig(api)
	if err != nil || updated.TLSCertificateFile == original.TLSCertificateFile || updated.TLSPrivateKeyFile == original.TLSPrivateKeyFile {
		t.Fatal("没有成对切换 HTTPS 身份", err)
	}
	certificate := readServerFixture(t, installer, updated.TLSCertificateFile)
	if bytes.Equal(certificate, preserved[original.TLSCertificateFile]) {
		t.Fatal("HTTPS 证书没有重新签发")
	}
	if err := installer.checkHTTPSUpgrade(updated, api, false); err != nil {
		t.Fatal("新证书仍不兼容浏览器", err)
	}
	if _, err := installer.fs.inspect(fileRule{path: updated.TLSPrivateKeyFile, private: true, api: true, maximum: 64 << 10}, api); err != nil {
		t.Fatal("新私钥权限不正确", err)
	}
	host.failStart = false
	// 恢复读取已保存的重新签发意图，不要求用户再次添加参数。
	report, err = installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
	if err != nil || !report.Healthy || host.backups != 1 || report.CertificateFile != updated.TLSCertificateFile {
		t.Fatal("HTTPS 重新签发未完成恢复", report, err)
	}
	if !bytes.Equal(certificate, readServerFixture(t, installer, updated.TLSCertificateFile)) {
		t.Fatal("恢复时重复签发，改变了已发布证书")
	}
	for path, content := range preserved {
		if !bytes.Equal(content, readServerFixture(t, installer, path)) {
			t.Fatal("覆盖了原身份文件", path)
		}
	}
	record, _, err := installer.loadServerRecord()
	if err != nil {
		t.Fatal(err)
	}
	initial, err := serverapp.ParseConfig(record.InitialConfig)
	if err != nil || initial.TLSCertificateFile != updated.TLSCertificateFile || initial.TLSPrivateKeyFile != updated.TLSPrivateKeyFile {
		t.Fatal("安装恢复记录未同步 HTTPS 引用", err)
	}
	if _, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd"); err != nil || host.backups != 1 {
		t.Fatal("重复升级破坏幂等性", err)
	}
}

func TestServerUpgradeChecksHTTPSBeforeStoppingService(t *testing.T) {
	for _, caSigned := range []bool{false, true} {
		installer, host := serverInstallFixture(t)
		installServerFixture(t, installer)
		installLegacyHTTPSFixture(t, installer, caSigned)
		candidate := readServerFixture(t, installer, serverBinaryPath)
		candidate[len(candidate)-1] = 8
		writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
		_, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{RenewHTTPS: caSigned}, "/source/new-tomd")
		if err == nil || !host.active || host.backups != 0 {
			t.Fatal("未拒绝未授权的 HTTPS 替换或影响了原服务", err)
		}
		if _, err := os.Stat(installer.fs.resolve(serverUpgradePath)); !os.IsNotExist(err) {
			t.Fatal("预检失败仍创建了升级记录", err)
		}
	}
}
