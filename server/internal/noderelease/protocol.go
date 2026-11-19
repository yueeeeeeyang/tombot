package noderelease

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
)

const AgentPath = "/api/v2/node-update-agent/package"

type Call struct {
	Node     string `json:"node"`
	Platform string `json:"platform"`
	Action   string `json:"action"`
	Job      string `json:"job,omitempty"`
	Index    int    `json:"index,omitempty"`
	State    string `json:"state,omitempty"`
	Issued   int64  `json:"issued"`
}
type SignedCall struct {
	Call        Call   `json:"call"`
	Certificate []byte `json:"certificate"`
	Signature   []byte `json:"signature"`
}
type Assignment struct {
	Manifest Manifest `json:"manifest"`
	Job      string   `json:"job"`
	Release  string   `json:"release"`
	Artifact string   `json:"artifact"`
}

func (c Call) message() ([]byte, error) {
	b, err := json.Marshal(c)
	return append([]byte("tombot-node-update-v1\n"), b...), err
}
func (c Call) Sign(pair tls.Certificate) (SignedCall, error) {
	key, ok := pair.PrivateKey.(ed25519.PrivateKey)
	if !ok || len(pair.Certificate) != 1 {
		return SignedCall{}, errors.New("节点证书无效")
	}
	c.Issued = time.Now().Unix()
	body, err := c.message()
	if err != nil {
		return SignedCall{}, err
	}
	return SignedCall{c, pair.Certificate[0], ed25519.Sign(key, body)}, nil
}
func (s SignedCall) Valid(now time.Time) bool {
	c := s.Call
	cert, err := x509.ParseCertificate(s.Certificate)
	if err != nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) || c.Issued < now.Add(-time.Minute).Unix() || c.Issued > now.Add(time.Minute).Unix() || (computer.NodeQuery{NodeID: c.Node}).Validate() != nil || (c.Platform != "linux/amd64" && c.Platform != "linux/arm64") {
		return false
	}
	body, err := c.message()
	key, ok := cert.PublicKey.(ed25519.PublicKey)
	return ok && err == nil && ed25519.Verify(key, body, s.Signature)
}
