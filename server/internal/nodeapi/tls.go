package nodeapi

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Fingerprint 是证书叶证书 DER 的 SHA-256。
// 跨主机后不再有内核凭据可用，双方改为固定对方证书指纹：信任来自配置而不是证书链，
// 因此不需要维护 CA，轮换也能做到先加新指纹再删旧指纹。
type Fingerprint [sha256.Size]byte

// ParseFingerprints 只接受规范小写十六进制；重复项直接拒绝，避免轮换时出现歧义。
func ParseFingerprints(values []string) ([]Fingerprint, error) {
	if len(values) == 0 || len(values) > 8 {
		return nil, errors.New("节点证书指纹数量无效")
	}
	fingerprints := make([]Fingerprint, 0, len(values))
	seen := make(map[Fingerprint]bool, len(values))
	for _, value := range values {
		if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
			return nil, errors.New("节点证书指纹格式无效")
		}
		decoded, err := hex.DecodeString(value)
		if err != nil {
			return nil, errors.New("节点证书指纹格式无效")
		}
		var fingerprint Fingerprint
		copy(fingerprint[:], decoded)
		if seen[fingerprint] {
			return nil, errors.New("节点证书指纹重复")
		}
		seen[fingerprint] = true
		fingerprints = append(fingerprints, fingerprint)
	}
	return fingerprints, nil
}

func fingerprintOf(encoded []byte) Fingerprint { return sha256.Sum256(encoded) }

// verifyLeaf 同时核对指纹集合与有效期。
// 指纹固定替代了证书链校验，链校验原本附带的有效期检查必须在这里补回，否则过期证书仍可用。
func verifyLeaf(encoded []byte, allowed map[Fingerprint]bool, now time.Time) error {
	leaf, err := x509.ParseCertificate(encoded)
	if err != nil {
		return errors.New("对端证书无法解析")
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("对端证书不在有效期内")
	}
	if !allowed[fingerprintOf(leaf.Raw)] {
		return errors.New("对端证书指纹不在允许列表")
	}
	return nil
}

// parseLeaf 在构造阶段检查本机证书；运行期核验由 TLS 回调负责。
func parseLeaf(certificate tls.Certificate) (*x509.Certificate, error) {
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("节点证书链为空")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("节点证书无法解析")
	}
	return leaf, nil
}

func allowedSet(fingerprints []Fingerprint) map[Fingerprint]bool {
	set := make(map[Fingerprint]bool, len(fingerprints))
	for _, fingerprint := range fingerprints {
		set[fingerprint] = true
	}
	return set
}

// ServerTLS 供节点使用：要求客户端出示证书，只接受配置中的指纹，最低 TLS 1.3。
func ServerTLS(certificate tls.Certificate, fingerprints []Fingerprint) (*tls.Config, error) {
	leaf, err := parseLeaf(certificate)
	if err != nil {
		return nil, err
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("节点证书指纹数量无效")
	}
	allowed := allowedSet(fingerprints)
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("节点证书不在有效期内")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// 自签证书没有公共信任链：只要求出示证书，是否接受由指纹决定。
		ClientAuth: tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) != 1 {
				return errors.New("节点请求方未提供唯一证书")
			}
			return verifyLeaf(raw[0], allowed, time.Now())
		},
	}, nil
}

// ClientTLS 供 API 使用：出示自己的证书，并只接受配置中的节点证书指纹。
func ClientTLS(certificate tls.Certificate, fingerprints []Fingerprint) (*tls.Config, error) {
	leaf, err := parseLeaf(certificate)
	if err != nil {
		return nil, err
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("节点证书指纹数量无效")
	}
	allowed := allowedSet(fingerprints)
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("客户端证书不在有效期内")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// 指纹固定替代链校验，因此关闭默认校验并在回调里同时核对指纹与有效期。
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) != 1 {
				return errors.New("节点未提供唯一证书")
			}
			return verifyLeaf(raw[0], allowed, time.Now())
		},
	}, nil
}

// FingerprintOf 返回证书叶证书的指纹文本，供部署工具比对两端配置。
func FingerprintOf(certificate tls.Certificate) (string, error) {
	leaf, err := parseLeaf(certificate)
	if err != nil {
		return "", err
	}
	fingerprint := fingerprintOf(leaf.Raw)
	return hex.EncodeToString(fingerprint[:]), nil
}
