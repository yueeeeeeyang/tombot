package deployment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"io.yueyang/tom/internal/serverapp"
)

func (installer serverInstaller) status(ctx context.Context) (report ServerReport, result error) {
	record, exists, err := installer.loadServerRecord()
	if err != nil {
		return report, err
	}
	if !exists {
		return ServerReport{Service: "未安装"}, errors.New("尚未通过 tomd install 安装")
	}
	api, err := installer.account(ctx, false)
	if err != nil || api.uid != record.UID || api.gid != record.GID {
		return report, errors.New("服务账户与安装记录不一致")
	}
	configuration, err := installer.loadServerConfig(api)
	if err != nil {
		return report, err
	}
	report.URL = configuration.AdminOrigin + "/admin/"
	for _, target := range []struct{ path, digest string }{{serverBinaryPath, record.BinarySHA256}, {serverServicePath, record.ServiceSHA256}} {
		content, err := installer.fs.readServerFile(fileRule{path: target.path, maximum: maximumNodeBinaryBytes}, account{})
		if err != nil || contentDigest(content) != target.digest {
			return report, errors.New("已安装程序或服务单元与安装记录不一致，检查是否有未完成升级")
		}
	}
	encoded, err := installer.fs.readServerFile(fileRule{path: configuration.TLSCertificateFile, maximum: 1 << 20, api: true}, api)
	if err != nil {
		return report, err
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		return report, errors.New("服务证书无法读取")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return report, errors.New("服务证书无法解析")
	}
	report.SelfSigned = leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
	report.CertificateExpires = leaf.NotAfter.UTC().Format(time.RFC3339)
	report.CertificateFile, report.CertificateSHA256 = configuration.TLSCertificateFile, contentDigest(leaf.Raw)
	properties, err := installer.runSystemctl(ctx, 15*time.Second, "show", "tomd.service", "--property=ActiveState,UnitFileState,Result")
	if err != nil {
		return report, err
	}
	state := parseServiceProperties(properties)
	report.Service, report.Enabled = state["ActiveState"], state["UnitFileState"] == "enabled"
	if report.Service != "active" || state["Result"] != "success" {
		result = errors.Join(result, errors.New("tomd 服务未正常运行"))
	}
	if !report.Enabled {
		result = errors.Join(result, errors.New("tomd 尚未设置开机启动"))
	}
	if err := installer.health(ctx, configuration); err != nil {
		result = errors.Join(result, err)
	} else {
		report.Healthy = true
	}
	return report, result
}

func parseServiceProperties(encoded []byte) map[string]string {
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = value
		}
	}
	return properties
}

// checkServerHealth 直接检查本机监听端口，同时核验证书及服务域名，不依赖公网 DNS 或代理。
func (installer serverInstaller) checkServerHealth(ctx context.Context, configuration serverapp.Config) (result error) {
	api, err := installer.account(ctx, false)
	if err != nil {
		return err
	}
	leaf, err := installer.serverCertificate(configuration, api)
	if err != nil {
		return err
	}
	if err := validateBrowserCertificate(leaf); err != nil {
		return err
	}
	certificate, err := installer.fs.readServerFile(fileRule{path: configuration.TLSCertificateFile, maximum: 1 << 20, api: true}, api)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return errors.New("健康检查无法载入服务证书")
	}
	origin, err := url.Parse(configuration.AdminOrigin)
	if err != nil {
		return errors.New("健康检查来源无效")
	}
	host, port, err := net.SplitHostPort(configuration.Address)
	if err != nil {
		return errors.New("健康检查监听地址无效")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: origin.Hostname(), MinVersion: tls.VersionTLS13}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, configuration.AdminOrigin+"/health/ready", nil)
	if err != nil {
		return errors.New("构造健康检查请求失败")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("本机 HTTPS 健康检查连接失败")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			result = errors.Join(result, errors.New("关闭健康检查连接失败"))
		}
	}()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(encoded) > 4096 || response.StatusCode != http.StatusOK {
		return errors.New("服务端或数据库尚未就绪")
	}
	var health struct {
		Status string `json:"status"`
		Scope  string `json:"scope"`
	}
	if json.Unmarshal(encoded, &health) != nil || health.Status != "ready" || health.Scope != "api" {
		return errors.New("健康检查响应不是 TomBot API 就绪状态")
	}
	return nil
}
