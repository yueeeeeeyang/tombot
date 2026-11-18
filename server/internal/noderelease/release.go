// Package noderelease 校验发布方签名的离线包分片；管理平台不能自行签发宿主程序。
package noderelease

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
)

// 每个 HTTP 请求只传一个分片，以适应服务端统一的请求预算。
const ChunkSize = 4 << 20
const MaxSize int64 = 17 << 30
const KeyPath = "/etc/tombot/node-release.pub"
const MaxEnvelope = 600 << 10

var digestPattern = regexp.MustCompile("^[0-9a-f]{64}$")
var labelPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$")

type Envelope struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}
type Manifest struct {
	Checksums string   `json:"checksums"`
	Version   int      `json:"version"`
	Label     string   `json:"label"`
	Platform  string   `json:"platform"`
	Size      int64    `json:"size"`
	Chunks    []string `json:"chunks"`
}

func Digest(content []byte) string  { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }
func ValidDigest(value string) bool { return digestPattern.MatchString(value) }
func ReadKey() (ed25519.PublicKey, error) {
	body, err := os.ReadFile(KeyPath)
	if err != nil {
		return nil, errors.New("未配置发布公钥 " + KeyPath)
	}
	return ParseKey(body)
}
func ParseKey(body []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(body)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("发布公钥格式无效")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	public, ok := key.(ed25519.PublicKey)
	if err != nil || !ok {
		return nil, errors.New("发布公钥必须为 Ed25519")
	}
	return public, nil
}

// Verify 先核验独立发布公钥，再接受平台、大小与有序分片摘要。
func (e Envelope) Verify(key ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	if len(key) != ed25519.PublicKeySize || len(e.Payload) > MaxEnvelope || !ed25519.Verify(key, e.Payload, e.Signature) || json.Unmarshal(e.Payload, &m) != nil || m.Version != 1 || !ValidDigest(m.Checksums) || !labelPattern.MatchString(m.Label) || (m.Platform != "linux/amd64" && m.Platform != "linux/arm64") || m.Size <= 0 || m.Size > MaxSize || int64(len(m.Chunks)) != (m.Size+ChunkSize-1)/ChunkSize {
		return Manifest{}, errors.New("发布签名、平台或分片清单无效")
	}
	for _, d := range m.Chunks {
		if !ValidDigest(d) {
			return Manifest{}, errors.New("发布包分片摘要无效")
		}
	}
	return m, nil
}
func (m Manifest) PartSize(index int) int64 { return min(ChunkSize, m.Size-int64(index)*ChunkSize) }
