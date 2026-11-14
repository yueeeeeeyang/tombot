package deployment

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/serverapp"
	"io.yueyang/tom/internal/store/postgres"
)

// NormalizeServerURL 约束单一 HTTPS 来源，不把路径、用户信息或查询串写入配置。
func NormalizeServerURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("访问地址须为 https://域名或IP:端口，不带路径或结尾斜杠")
	}
	if strings.ContainsAny(parsed.Hostname(), " \t\r\n%/\\") || parsed.Hostname() == "" {
		return "", errors.New("服务访问主机无效")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && (ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast()) {
		return "", errors.New("服务访问地址必须是客户端可达的明确地址")
	}
	if parsed.Port() == "" {
		parsed.Host = net.JoinHostPort(parsed.Hostname(), "9443")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		return "", errors.New("直连服务端口须在 1024 至 65535 之间，默认 9443")
	}
	return parsed.String(), nil
}

// SuggestedServerURL 从本机非回环网卡给出建议，不查询公网服务，最终由安装者确认。
func SuggestedServerURL() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", errors.New("读取网卡失败，请显式提供 --url")
	}
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0 || strings.HasPrefix(network.Name, "docker") || strings.HasPrefix(network.Name, "br-") || strings.HasPrefix(network.Name, "veth") {
			continue
		}
		addresses, err := network.Addrs()
		if err != nil {
			return "", errors.New("读取网卡地址失败，请显式提供 --url")
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() {
				return "https://" + net.JoinHostPort(ip.String(), "9443"), nil
			}
		}
	}
	return "", errors.New("未检测到可用 IPv4 地址，请用 --url 指定域名或 IPv6 地址")
}

func (installer serverInstaller) prepareServerFiles(ctx context.Context, options ServerInstallOptions, api account, program []byte) (record serverInstallation, files []configurationFile, result error) {
	record = serverInstallation{Version: 1, UID: api.uid, GID: api.gid, BinarySHA256: contentDigest(program), ServiceSHA256: contentDigest([]byte(deploy.ServerService())), ManagedDatabase: options.Config == ""}
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return record, nil, errors.New("生成服务安装标识失败")
	}
	record.ID = hex.EncodeToString(identifier)
	if options.Config != "" {
		if options.URL != "" || options.Certificate != "" || options.PrivateKey != "" {
			return record, nil, errors.New("--config 与自动配置参数不能同时使用")
		}
		encoded, err := installer.fs.readInstallationSource(options.Config, 128<<10, installer.caller)
		if err != nil {
			return record, nil, err
		}
		if _, err := serverapp.ParseConfig(encoded); err != nil {
			return record, nil, err
		}
		record.InitialConfig = encoded
		return record, nil, nil
	}
	if (options.Certificate == "") != (options.PrivateKey == "") {
		return record, nil, errors.New("正式证书必须同时提供 --certificate 和 --private-key")
	}
	address := options.URL
	if address == "" {
		if installer.confirmURL == nil {
			return record, nil, errors.New("需要确认访问地址，或通过 --url 指定")
		}
		suggestion, err := SuggestedServerURL()
		if err != nil {
			return record, nil, err
		}
		address, err = installer.confirmURL(ctx, suggestion)
		if err != nil {
			return record, nil, err
		}
	}
	address, err := NormalizeServerURL(address)
	if err != nil {
		return record, nil, err
	}
	origin, err := url.Parse(address)
	if err != nil {
		return record, nil, errors.New("服务访问地址无效")
	}
	configuration := serverapp.DefaultConfig()
	configuration.Address, configuration.AdminOrigin = net.JoinHostPort("0.0.0.0", origin.Port()), address
	if ip := net.ParseIP(origin.Hostname()); ip != nil && ip.To4() == nil {
		configuration.Address = net.JoinHostPort("::", origin.Port())
	}
	configuration.TLSCertificateFile, configuration.TLSPrivateKeyFile = serverDirectory+"/certificate.pem", serverDirectory+"/tls.key"
	configuration.IdentityKeyFile, configuration.CSRFKeyFile = serverDirectory+"/identity.key", serverDirectory+"/csrf.key"
	configuration.ActiveModelKey, configuration.ModelKeyFiles = "current", map[string]string{"current": serverDirectory + "/model.key"}
	configuration.Node.CertificateFile, configuration.Node.PrivateKeyFile = serverDirectory+"/api-certificate.pem", serverDirectory+"/api.key"
	for _, name := range []string{"identity.key", "csrf.key", "model.key"} {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return record, files, errors.New("生成服务加密材料失败")
		}
		files = append(files, configurationFile{name: name, content: key, mode: 0600})
	}
	certificate, privateKey, err := createServerCertificate(origin.Hostname(), false, time.Now())
	if err != nil {
		return record, files, fmt.Errorf("准备 HTTPS 服务身份失败：%w", err)
	}
	if options.Certificate != "" {
		clear(privateKey)
		certificate, err = installer.fs.readInstallationSource(options.Certificate, 1<<20, installer.caller)
		if err != nil {
			return record, files, err
		}
		privateKey, err = installer.fs.readInstallationSource(options.PrivateKey, 64<<10, installer.caller)
		if err != nil {
			return record, files, err
		}
		info, err := os.Lstat(installer.fs.resolve(options.PrivateKey))
		if err != nil || info.Mode().Perm()&0077 != 0 {
			clear(privateKey)
			return record, files, errors.New("输入的 HTTPS 私钥必须仅所有者可读写，请先 chmod 600")
		}
	}
	files = append(files, configurationFile{name: "certificate.pem", content: certificate, mode: 0644}, configurationFile{name: "tls.key", content: privateKey, mode: 0600})
	leaf, err := validateServerCertificate(certificate, privateKey, origin.Hostname(), false)
	if err != nil {
		return record, files, err
	}
	if err := validateBrowserCertificate(leaf); err != nil {
		return record, files, err
	}
	certificate, privateKey, err = createServerCertificate("tombot-api", true, time.Now())
	if err != nil {
		return record, files, fmt.Errorf("准备节点通信客户端身份失败：%w", err)
	}
	files = append(files, configurationFile{name: "api-certificate.pem", content: certificate, mode: 0644}, configurationFile{name: "api.key", content: privateKey, mode: 0600})
	configuration.Database.MigrationDSNFile = serverDirectory + "/migration.connection"
	configuration.Database.DSNFiles = map[postgres.Role]string{}
	for _, purpose := range serverDatabasePurposes() {
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return record, files, errors.New("生成数据库认证材料失败")
		}
		connection := url.URL{Scheme: "postgres", User: url.UserPassword("tombot_"+purpose+"_login", hex.EncodeToString(password)), Host: "127.0.0.1:5432", Path: "/tombot", RawQuery: "sslmode=disable"}
		clear(password)
		files = append(files, configurationFile{name: purpose + ".connection", content: []byte(connection.String()), mode: 0600})
		if purpose != "migration" {
			configuration.Database.DSNFiles[postgres.Role("tombot_"+purpose)] = serverDirectory + "/" + purpose + ".connection"
		}
	}
	record.InitialConfig, err = json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return record, files, errors.New("编码服务配置失败")
	}
	if _, err := serverapp.ParseConfig(record.InitialConfig); err != nil {
		return record, files, err
	}
	return record, files, nil
}

func createServerCertificate(host string, client bool, now time.Time) ([]byte, []byte, error) {
	var public crypto.PublicKey
	var private crypto.Signer
	if client {
		clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, errors.New("生成节点通信客户端身份失败")
		}
		defer clear(clientPrivate)
		public, private = clientPublic, clientPrivate
	} else {
		// 浏览器不协商 Ed25519 服务端签名；HTTPS 身份使用通用的 P-256，节点通信身份独立保存。
		serverPrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, errors.New("生成 HTTPS 服务身份失败")
		}
		public, private = &serverPrivate.PublicKey, serverPrivate
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errors.New("生成证书序号失败")
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if client {
		certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else if ip := net.ParseIP(host); ip != nil {
		certificate.IPAddresses = []net.IP{ip}
	} else {
		certificate.DNSNames = []string{host}
	}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		// 标准库错误描述签发阶段，不包含私钥；保留原因才能定位宿主加密实现或证书编码错误。
		return nil, nil, fmt.Errorf("签发服务证书失败：%w", err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, errors.New("编码服务私钥失败")
	}
	defer clear(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), nil
}

// validateBrowserCertificate 在安装与健康检查中约束浏览器支持的服务端签名，避免仅 Go 客户端握手成功的假就绪。
func validateBrowserCertificate(certificate *x509.Certificate) error {
	switch public := certificate.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if public.Curve == elliptic.P256() || public.Curve == elliptic.P384() {
			return nil
		}
	case *rsa.PublicKey:
		if public.N.BitLen() >= 2048 {
			return nil
		}
	}
	return errors.New("HTTPS 证书算法与管理端浏览器不兼容，请使用 ECDSA P-256/P-384 或 RSA 证书；自签名证书可用 upgrade --renew-https 重新签发")
}

func validateServerCertificate(certificate, key []byte, host string, client bool) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, errors.New("证书与私钥不匹配")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("证书无效或已过期")
	}
	if !client && leaf.VerifyHostname(host) != nil {
		return nil, errors.New("HTTPS 证书与服务访问地址不匹配")
	}
	usage := x509.ExtKeyUsageServerAuth
	if client {
		usage = x509.ExtKeyUsageClientAuth
	}
	if len(leaf.ExtKeyUsage) != 0 {
		valid := false
		for _, actual := range leaf.ExtKeyUsage {
			valid = valid || actual == usage || actual == x509.ExtKeyUsageAny
		}
		if !valid {
			return nil, errors.New("证书用途不符合服务身份")
		}
	}
	return leaf, nil
}

func serverReferences(configuration serverapp.Config) []fileRule {
	references := []fileRule{
		{path: configuration.TLSCertificateFile, maximum: 1 << 20, api: true}, {path: configuration.TLSPrivateKeyFile, maximum: 64 << 10, private: true, api: true},
		{path: configuration.IdentityKeyFile, maximum: 32, exact: 32, private: true, api: true}, {path: configuration.CSRFKeyFile, maximum: 32, exact: 32, private: true, api: true},
		{path: configuration.Node.CertificateFile, maximum: 1 << 20, api: true}, {path: configuration.Node.PrivateKeyFile, maximum: 64 << 10, private: true, api: true},
		{path: configuration.Database.MigrationDSNFile, maximum: 16 << 10, private: true, api: true},
	}
	for _, path := range configuration.Database.DSNFiles {
		references = append(references, fileRule{path: path, maximum: 16 << 10, private: true, api: true})
	}
	for _, path := range configuration.ModelKeyFiles {
		references = append(references, fileRule{path: path, maximum: 32, exact: 32, private: true, api: true})
	}
	if configuration.Models.RootCAFile != "" {
		references = append(references, fileRule{path: configuration.Models.RootCAFile, maximum: 1 << 20, api: true})
	}
	return references
}

func (installer serverInstaller) loadServerConfig(api account) (serverapp.Config, error) {
	encoded, err := installer.fs.readServerFile(fileRule{path: serverConfigPath, maximum: 128 << 10, api: true}, api)
	if err != nil {
		return serverapp.Config{}, err
	}
	configuration, err := serverapp.ParseConfig(encoded)
	if err != nil {
		return configuration, err
	}
	contents := map[string][]byte{}
	defer func() {
		for _, content := range contents {
			clear(content)
		}
	}()
	for _, rule := range serverReferences(configuration) {
		content, err := installer.fs.readServerFile(rule, api)
		if err != nil {
			return configuration, err
		}
		contents[rule.path] = content
	}
	origin, err := url.Parse(configuration.AdminOrigin)
	if err != nil {
		return configuration, errors.New("管理来源无效")
	}
	if _, err := validateServerCertificate(contents[configuration.TLSCertificateFile], contents[configuration.TLSPrivateKeyFile], origin.Hostname(), false); err != nil {
		return configuration, err
	}
	if _, err := validateServerCertificate(contents[configuration.Node.CertificateFile], contents[configuration.Node.PrivateKeyFile], "", true); err != nil {
		return configuration, err
	}
	connections := []string{strings.TrimSpace(string(contents[configuration.Database.MigrationDSNFile]))}
	for _, path := range configuration.Database.DSNFiles {
		connections = append(connections, strings.TrimSpace(string(contents[path])))
	}
	if err := postgres.ValidateSingleDatabase(connections); err != nil {
		return configuration, err
	}
	return configuration, nil
}

// readServerFile 允许读取已核验的服务账户私有引用，复用文件身份和读中替换保护。
func (fs filesystem) readServerFile(rule fileRule, api account) ([]byte, error) {
	info, err := fs.inspect(rule, api)
	if err != nil {
		return nil, err
	}
	return fs.readInspectedFile(rule, info)
}
