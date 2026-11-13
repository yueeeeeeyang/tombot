package serverapp

import (
	"crypto/tls"
	"errors"

	"io.yueyang/tom/internal/nodeapi"
)

// newNodeClient 构造跨主机节点客户端。
// 证书由部署分别签发给 API 与节点，双方各自固定对方指纹，不依赖共享 CA 或同机内核身份。
func newNodeClient(configuration NodeConfig) (*nodeapi.Client, error) {
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
	fingerprints, err := nodeapi.ParseFingerprints(configuration.ServerFingerprints)
	if err != nil {
		return nil, err
	}
	clientTLS, err := nodeapi.ClientTLS(pair, fingerprints)
	if err != nil {
		return nil, err
	}
	return nodeapi.NewClient(nodeapi.ClientOptions{NodeID: configuration.ID, URL: configuration.URL, CallTimeout: configuration.CallTimeout.Value(), TLS: clientTLS})
}
