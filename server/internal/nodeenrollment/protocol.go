// Package nodeenrollment 提供一次性接入授权与已绑定节点的签名登记，不使用管理员会话运行节点。
package nodeenrollment

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodecatalog"
)

const Path = "/api/v2/node-enrollment"
const maximumBodyBytes = 16 << 10
const signatureLifetime = 5 * time.Minute

var (
	ErrUnauthorized = errors.New("节点接入凭据、证书或签名无效")
	ErrConflict     = errors.New("节点接入身份与现有登记冲突")
	ErrCapacity     = errors.New("服务端节点数量已达到上限")
)

// ServerConfig 只保存地址、公开指纹和私有凭据文件引用。
type ServerConfig struct {
	URL          string   `json:"url"`
	Fingerprints []string `json:"certificate_fingerprints"`
	TokenFile    string   `json:"enrollment_token_file"`
	NodeURL      string   `json:"node_url"`
}

// Config 由安装器绑定节点元数据，启动后用同一身份自动登记。
type Config struct {
	Server       ServerConfig             `json:"server"`
	Registration nodecatalog.Registration `json:"registration"`
}

func (configuration Config) Validate() error {
	server := configuration.Server
	if nodeapi.ValidateURL(server.URL) != nil || nodeapi.ValidateURL(server.NodeURL) != nil || configuration.Registration.Validate() != nil || !filepath.IsAbs(server.TokenFile) || filepath.Clean(server.TokenFile) != server.TokenFile || server.TokenFile == "/" || strings.ContainsAny(server.TokenFile, "\x00\r\n") {
		return errors.New("节点自动登记配置无效")
	}
	_, err := nodeapi.ParseFingerprints(server.Fingerprints)
	return err
}

// Claim 的全部字段由节点私钥签名，重试仅更新时间，登记身份始终不变。
type Claim struct {
	Maintenance              *MaintenanceRequest      `json:"maintenance,omitempty"`
	NodeID                   string                   `json:"node_id"`
	Registration             nodecatalog.Registration `json:"registration"`
	URL                      string                   `json:"url"`
	Certificate              []byte                   `json:"certificate"`
	IssuedAt                 int64                    `json:"issued_at"`
	SupportsComputerUpgrades bool                     `json:"supports_computer_upgrades,omitempty"`
}

type Request struct {
	Claim     Claim  `json:"claim"`
	Signature string `json:"signature"`
}

type Receipt struct {
	Recovery *RecoveryInspection `json:"recovery,omitempty"`
	NodeID   string              `json:"node_id"`
	ImageID  string              `json:"image_id"`
	State    string              `json:"state"`
}

// Verify 同时检查新鲜度、证书有效期和私钥持有证明；公开证书本身不构成接入授权。
func (request Request) Verify(now time.Time) (string, error) {
	claim := request.Claim
	if (computer.NodeQuery{NodeID: claim.NodeID}).Validate() != nil || claim.Registration.Validate() != nil || nodeapi.ValidateURL(claim.URL) != nil || len(claim.Certificate) > 4096 || claim.IssuedAt < now.Add(-signatureLifetime).Unix() || claim.IssuedAt > now.Add(signatureLifetime).Unix() {
		return "", ErrUnauthorized
	}
	certificate, err := x509.ParseCertificate(claim.Certificate)
	if err != nil || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return "", ErrUnauthorized
	}
	public, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", ErrUnauthorized
	}
	encoded, err := json.Marshal(claim)
	if err != nil {
		return "", ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Signature)
	if err != nil || !ed25519.Verify(public, encoded, signature) {
		return "", ErrUnauthorized
	}
	digest := sha256.Sum256(claim.Certificate)
	return hex.EncodeToString(digest[:]), nil
}
