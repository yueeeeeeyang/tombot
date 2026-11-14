package serverapp

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io.yueyang/tom/internal/nodeenrollment"
	"log/slog"

	"io.yueyang/tom/internal/nodeapi"
)

// newNodeClient 构造跨主机节点客户端。
// 证书由部署分别签发给 API 与节点，双方各自固定对方指纹，不依赖共享 CA 或同机内核身份。
func newNodeClient(configuration NodeConfig, resolver nodeapi.Resolver) (*nodeapi.Router, error) {
	certificate, err := readFile(configuration.CertificateFile, 1<<20, false)
	if err != nil {
		return nil, err
	}
	privateKey, err := readFile(configuration.PrivateKeyFile, 64<<10, true)
	if err != nil {
		return nil, err
	}
	defer clear(privateKey)
	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil {
		return nil, errors.New("节点客户端证书或私钥无效")
	}
	return nodeapi.NewRouter(resolver, pair, configuration.CallTimeout.Value(), configuration.MaximumNodes)
}

// newNodeBootstrap 直接使用部署中的证书，安装者无需再复制两组指纹。
func newNodeBootstrap(configuration Config, inbound *tls.Config, service *nodeenrollment.Service, logger *slog.Logger) (*nodeenrollment.Bootstrap, error) {
	if len(inbound.Certificates) == 0 {
		return nil, errors.New("节点引导缺少服务证书")
	}
	pin, err := nodeapi.FingerprintOf(inbound.Certificates[0])
	if err != nil {
		return nil, err
	}
	encoded, err := readFile(configuration.Node.CertificateFile, 1<<20, false)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("节点客户端证书无效")
	}
	clientPin, err := nodeapi.FingerprintOf(tls.Certificate{Certificate: [][]byte{block.Bytes}})
	if err != nil {
		return nil, err
	}
	return nodeenrollment.NewBootstrap(service, configuration.AdminOrigin, []string{pin}, nodeenrollment.BootstrapConfiguration{ClientFingerprints: []string{clientPin}, VerificationResources: configuration.Node.Verification.Resources}, logger)
}
