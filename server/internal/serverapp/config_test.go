package serverapp

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	directory := t.TempDir()
	configuration := DefaultConfig()
	configuration.Address = "127.0.0.1:8443"
	configuration.AdminOrigin = "https://127.0.0.1:8443"
	configuration.AdminDirectory = filepath.Join(directory, "admin")
	configuration.TLSCertificateFile = filepath.Join(directory, "certificate.pem")
	configuration.TLSPrivateKeyFile = filepath.Join(directory, "private.pem")
	configuration.IdentityKeyFile = filepath.Join(directory, "identity.key")
	configuration.CSRFKeyFile = filepath.Join(directory, "csrf.key")
	configuration.ActiveModelKey = "current"
	configuration.ModelKeyFiles = map[string]string{"current": filepath.Join(directory, "model.key")}
	configuration.Database.DSNFiles = map[postgres.Role]string{}
	for _, role := range []postgres.Role{postgres.IdentityRole, postgres.ControlRole, postgres.TenantRole, postgres.DispatchRole} {
		configuration.Database.DSNFiles[role] = filepath.Join(directory, string(role)+".connection")
	}
	configuration.Node.ID = ids.New()
	configuration.Node.Verification.ImageID = ids.New()
	configuration.Node.Socket = "/run/tombot/control.sock"
	configuration.Models.Origins = []Origin{{URL: "https://model.example.test"}}
	return configuration
}

// testCertificate 每次生成独立短期证书，测试不保存可复用凭据。
func testCertificate(t *testing.T, configuration Config, validFrom, validUntil time.Time) *x509.CertPool {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: validFrom, NotAfter: validUntil, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	writeTestFile(t, configuration.TLSCertificateFile, certificatePEM)
	writeTestFile(t, configuration.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	clear(private)
	clear(encoded)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("测试证书无法载入")
	}
	return roots
}

func TestLoadConfig(t *testing.T) {
	configuration := testConfig(t)
	path := filepath.Join(t.TempDir(), "server.json")
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, encoded)
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Node.ID != configuration.Node.ID || loaded.Runs.AgentTimeout.Value() != 5*time.Second {
		t.Fatalf("配置未正确载入: %v", err)
	}
	for _, fixture := range []struct {
		name    string
		content []byte
	}{
		{"未知字段", append([]byte(`{"unknown":true,`), encoded[1:]...)},
		{"尾随对象", append(bytes.Clone(encoded), []byte(` {}`)...)},
		{"整数时限", bytes.Replace(encoded, []byte(`"agent_timeout":"5s"`), []byte(`"agent_timeout":5`), 1)},
		{"无界时限", bytes.Replace(encoded, []byte(`"agent_timeout":"5s"`), []byte(`"agent_timeout":"999999h"`), 1)},
		{"零时限", bytes.Replace(encoded, []byte(`"agent_timeout":"5s"`), []byte(`"agent_timeout":"0s"`), 1)},
		{"空配置", []byte(`null`)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			writeTestFile(t, path, fixture.content)
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("错误配置未拒绝")
			}
		})
	}
	for _, fixture := range []struct {
		name   string
		change func(*Config)
	}{
		{"明文来源", func(c *Config) { c.AdminOrigin = "http://127.0.0.1" }},
		{"心跳间隔缺失", func(c *Config) { c.Node.HeartbeatInterval = 0 }},
		{"心跳期限预算不足", func(c *Config) { c.Node.ControlLifetime = Duration(time.Second) }},
		{"缺少验收镜像", func(c *Config) { c.Node.Verification.ImageID = "" }},
		{"验收租约预算不足", func(c *Config) { c.Node.Verification.LeaseDuration = Duration(time.Second) }},
		{"验收证据时限不足", func(c *Config) { c.Node.Verification.Lifetime = Duration(time.Minute) }},
		{"验收重试无界", func(c *Config) { c.Node.Verification.MaxAttempts = 0 }},
		{"验收磁盘未限额", func(c *Config) { c.Node.Verification.Resources.DiskBytes = 0 }},
		{"来源包含路径", func(c *Config) { c.AdminOrigin += "/admin" }},
		{"缺少监听端口", func(c *Config) { c.Address = "127.0.0.1" }},
		{"自动监听端口", func(c *Config) { c.Address = "127.0.0.1:0" }},
		{"相对文件路径", func(c *Config) { c.IdentityKeyFile = "identity.key" }},
		{"截图额度不足一张", func(c *Config) { c.Runs.Images.MaxUserBytes = 1 }},
		{"截图数量无界", func(c *Config) { c.Runs.Images.MaxUserImages = 4097 }},
		{"截图上下文无界", func(c *Config) { c.Runs.Images.ContextImages = 5 }},
		{"截图保留时间不足", func(c *Config) { c.Runs.Images.Retention = Duration(time.Hour) }},
		{"视觉请求容量不足", func(c *Config) { c.Models.MaxRequestBytes = 2 << 20 }},
		{"桌面启动无界", func(c *Config) { c.Computer.DesktopStartTimeout = Duration(2 * time.Minute) }},
		{"缺少数据库用途", func(c *Config) { delete(c.Database.DSNFiles, postgres.DispatchRole) }},
		{"缺少当前加密文件", func(c *Config) { c.ActiveModelKey = "missing" }},
		{"HTTP 写入早于响应时限", func(c *Config) { c.HTTP.WriteTimeout = c.HTTP.RequestTimeout - 1 }},
		{"开户等待超出 HTTP", func(c *Config) { c.Provision.RequestWait = c.HTTP.RequestTimeout }},
		{"模型验证超出 HTTP", func(c *Config) { c.Models.VerificationTimeout = c.HTTP.RequestTimeout }},
		{"退出不够收尾", func(c *Config) { c.HTTP.ShutdownTimeout = c.Runs.RecordTimeout - 1 }},
		{"节点调用时限过短", func(c *Config) { c.Node.CallTimeout = Duration(time.Second) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			candidate := testConfig(t)
			fixture.change(&candidate)
			if candidate.validate() == nil {
				t.Fatal("矛盾配置未拒绝")
			}
		})
	}
}

func TestProtectedFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "protected")
	content := make([]byte, 32)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, content)
	key, err := readKey(path)
	if err != nil || !bytes.Equal(key, content) {
		t.Fatal("私有文件未正确读取", err)
	}
	clear(key)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("接受公开凭据文件")
	}
	if _, err := readFile(path, 32, false); err != nil {
		t.Fatal("拒绝只读公开配置", err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(path, 32, false); err == nil {
		t.Fatal("接受组可写配置")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(link, 32, true); err == nil {
		t.Fatal("接受符号链接")
	}
	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(path, 32, true); err == nil {
		t.Fatal("接受硬链接凭据")
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 31, 33, 1024} {
		writeTestFile(t, path, make([]byte, size))
		if _, err := readKey(path); err == nil {
			t.Fatalf("接受 %d 字节密钥", size)
		}
	}
	if _, err := readFile(directory, 32, false); err == nil {
		t.Fatal("接受目录")
	}
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := readFile(fifo, 32, true); result <- err }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("接受管道")
		}
	case <-time.After(time.Second):
		t.Fatal("读取管道阻塞启动")
	}
	_, err = readFile(filepath.Join(directory, "absent"), 32, true)
	if err == nil || strings.Contains(err.Error(), directory) {
		t.Fatal("配置错误未脱敏")
	}
}

func TestTLSConfiguration(t *testing.T) {
	configuration := testConfig(t)
	now := time.Now()
	if _, err := loadTLS(configuration); err == nil {
		t.Fatal("无证书仍可启动")
	}
	testCertificate(t, configuration, now.Add(-time.Hour), now.Add(time.Hour))
	inbound, err := loadTLS(configuration)
	if err != nil || inbound.MinVersion != tls.VersionTLS13 {
		t.Fatal("未强制 TLS 1.3", err)
	}
	configuration.AdminOrigin = "https://wrong.example.test"
	if _, err := loadTLS(configuration); err == nil {
		t.Fatal("接受来源不匹配证书")
	}
	configuration.AdminOrigin = "https://127.0.0.1"
	for _, period := range [][2]time.Time{{now.Add(-2 * time.Hour), now.Add(-time.Hour)}, {now.Add(time.Hour), now.Add(2 * time.Hour)}} {
		testCertificate(t, configuration, period[0], period[1])
		if _, err := loadTLS(configuration); err == nil {
			t.Fatal("接受未生效或过期证书")
		}
	}
	testCertificate(t, configuration, now.Add(-time.Hour), now.Add(time.Hour))
	writeTestFile(t, configuration.TLSPrivateKeyFile, []byte("无效文件"))
	if _, err := loadTLS(configuration); err == nil {
		t.Fatal("证书损坏时降级监听")
	}
}
