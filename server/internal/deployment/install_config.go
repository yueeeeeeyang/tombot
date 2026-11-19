package deployment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io.yueyang/tom/internal/infra/jsonshape"
	"math/big"
	"path/filepath"
	"reflect"
	"time"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapi"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/nodeenrollment"
)

const nodeCertificateLifetime = 365 * 24 * time.Hour
const nodeEnrollmentFile = "enrollment.token"

// nodeInstallServer 只包含公开接入信息，凭据由首次安装交互读取并存入私有目录。
type nodeInstallServer struct {
	URL          string   `json:"url"`
	Fingerprints []string `json:"certificate_fingerprints"`
	NodeURL      string   `json:"node_url"`
}

func (server nodeInstallServer) runtime() nodeenrollment.ServerConfig {
	return nodeenrollment.ServerConfig{URL: server.URL, Fingerprints: server.Fingerprints, NodeURL: server.NodeURL, TokenFile: filepath.Join(deploy.NodeDirectory, nodeEnrollmentFile)}
}

type nodeInstallControl struct {
	Listen             string   `json:"listen"`
	AllowPublic        bool     `json:"allow_public_listen"`
	ClientFingerprints []string `json:"client_fingerprints"`
}

// nodeInstallRequest 只包含公开部署参数与文件引用，私钥在节点首次安装时生成。
type nodeInstallRequest struct {
	Programs        programVersions    `json:"programs,omitzero"`
	PreviousImageID string             `json:"previous_image_id,omitempty"`
	Server          *nodeInstallServer `json:"server,omitempty"`
	Name            string             `json:"node_name,omitempty"`
	Capacity        int                `json:"computer_capacity,omitempty"`
	Version         int                `json:"version"`
	NodeID          string             `json:"node_id,omitempty"`
	ImageID         string             `json:"image_id,omitempty"`
	Control         nodeInstallControl `json:"control"`
	Image           imageSettings      `json:"image"`
	Docker          json.RawMessage    `json:"docker"`
	InstallOptions  json.RawMessage    `json:"install_options,omitempty"`
	Node            json.RawMessage    `json:"node,omitempty"`
}

// NodeInstallationReport 只公开接入主服务所需的节点身份，不包含私钥或认证内容。
type NodeInstallationReport struct {
	Scope                  string    `json:"scope"`
	State                  string    `json:"state"`
	NodeID                 string    `json:"node_id"`
	ImageID                string    `json:"image_id"`
	Listen                 string    `json:"listen"`
	CertificateFingerprint string    `json:"certificate_fingerprint"`
	CertificateExpiresAt   time.Time `json:"certificate_expires_at"`
}

type nodeInstallation struct {
	UpdateUnits   map[string]string `json:"update_units,omitempty"`
	ServiceSHA256 string            `json:"service_sha256,omitempty"`
	Version       int               `json:"version"`
	RequestSHA256 string            `json:"request_sha256"`
	BinarySHA256  string            `json:"binary_sha256"`
	Files         map[string]string `json:"files"`
	Request       json.RawMessage   `json:"request"`
}

// generatedNodeInstallRequest 仅输出节点差异选项；服务身份由接入码与已鉴权引导接口提供。
func generatedNodeInstallRequest(value configuration) ([]byte, error) {
	options := NodeInstallOptions{Listen: value.node.Listen}
	if value.node.Enrollment != nil {
		options.Name = value.node.Enrollment.Registration.Name
		options.Capacity = value.node.Enrollment.Registration.Capacity
		options.URL = value.node.Enrollment.Server.NodeURL
	}
	encoded, err := json.MarshalIndent(options, "", "  ")
	if err != nil {
		return nil, errors.New("节点高级选项无法编码")
	}
	return append(encoded, '\n'), nil
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func nodeInstallationPaths() Paths {
	return Paths{Node: filepath.Join(deploy.NodeDirectory, "node.json"), Docker: filepath.Join(deploy.NodeDirectory, "docker-install.json"), Image: filepath.Join(deploy.NodeDirectory, "computer-image.json")}
}

func parseNodeInstallRequest(encoded []byte) (nodeInstallRequest, string, error) {
	request := nodeInstallRequest{Image: imageSettings{CallTimeoutSeconds: 30, TimeoutSeconds: 600}}
	if decodePreparation(encoded, &request) != nil || request.Version != 1 {
		return request, "", errors.New("节点安装参数格式无效或版本不支持")
	}
	// 先规范化嵌套覆盖参数，重复安装不会因 JSON 缩进或字段顺序改变而生成新身份。
	for _, raw := range []*json.RawMessage{&request.Docker, &request.Node} {
		if len(*raw) == 0 {
			continue
		}
		var fields map[string]any
		decoder := json.NewDecoder(bytes.NewReader(*raw))
		decoder.UseNumber()
		if decoder.Decode(&fields) != nil {
			return request, "", errors.New("节点安装覆盖参数无效")
		}
		normalized, err := json.Marshal(fields)
		if err != nil {
			return request, "", errors.New("节点安装覆盖参数无法规范化")
		}
		*raw = normalized
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return request, "", errors.New("节点安装参数无法规范化")
	}
	return request, contentDigest(canonical), nil
}

func renderNodeInstallation(request nodeInstallRequest, platform string) (configuration, []configurationFile, error) {
	node, err := nodeapp.ParseConfig(deploy.NodeExample())
	if err != nil {
		return configuration{}, nil, errors.New("节点默认配置不可用")
	}
	if request.NodeID == "" {
		request.NodeID = ids.New()
	}
	if request.ImageID == "" {
		request.ImageID = ids.New()
	}
	node.NodeID, node.Listen, node.AllowPublic = request.NodeID, request.Control.Listen, request.Control.AllowPublic
	node.Control = nodeapp.ControlTLS{CertificateFile: filepath.Join(deploy.NodeDirectory, "certificate.pem"), PrivateKeyFile: filepath.Join(deploy.NodeDirectory, "private.key"), ClientFingerprints: request.Control.ClientFingerprints}
	if request.Server != nil {
		node.Enrollment = &nodeenrollment.Config{Server: request.Server.runtime(), Registration: nodecatalog.Registration{PreviousImageID: request.PreviousImageID, Name: request.Name, Capacity: request.Capacity, ImageID: request.ImageID, ImageReference: request.Image.Reference}}
	} else if request.Name != "" || request.Capacity != 0 {
		return configuration{}, nil, errors.New("登记信息必须同时配置服务端接入信息")
	}
	var enrollment *nodeenrollment.Config
	if node.Enrollment != nil {
		snapshot := *node.Enrollment
		snapshot.Server.Fingerprints = append([]string(nil), snapshot.Server.Fingerprints...)
		enrollment = &snapshot
	}
	control := node.Control
	if len(request.Node) != 0 && decodePreparation(request.Node, &node) != nil {
		return configuration{}, nil, errors.New("节点运行参数无效")
	}
	if node.NodeID != request.NodeID || node.Listen != request.Control.Listen || node.AllowPublic != request.Control.AllowPublic || !reflect.DeepEqual(node.Control, control) || !reflect.DeepEqual(node.Enrollment, enrollment) {
		return configuration{}, nil, errors.New("运行参数不能覆盖节点身份、监听地址或安装器管理的证书")
	}
	if _, err := nodeapi.ParseListenAddress(node.Listen, node.AllowPublic); err != nil {
		return configuration{}, nil, err
	}
	if request.Programs != (programVersions{}) && !request.Programs.valid() {
		return configuration{}, nil, errors.New("节点程序安装记录无效")
	}
	node.Runtime.ProgramCore = request.Programs.Core
	docker, err := dockerbootstrap.ParseManifest(request.Docker)
	if err != nil || jsonshape.Validate(request.Docker, &dockerbootstrap.Manifest{}) != nil {
		return configuration{}, nil, errors.New("节点安装需要有效的 Docker 引导清单")
	}
	image := dockernode.ImageManifest{ArchiveFile: request.Image.ArchiveFile, ArchiveSHA256: request.Image.ArchiveSHA256, Version: 1, NodeID: request.NodeID, ImageID: request.ImageID, Reference: request.Image.Reference, Platform: platform, Socket: dockerbootstrap.SocketPath, StateDirectory: "/var/lib/tombot-node/images", AuthenticationFile: request.Image.AuthenticationFile, CallTimeoutSeconds: request.Image.CallTimeoutSeconds, TimeoutSeconds: request.Image.TimeoutSeconds}
	value := configuration{node: node, image: image}
	files := []configurationFile{}
	for _, item := range []struct {
		name     string
		body     any
		validate func([]byte) error
	}{
		{"node.json", node, func(content []byte) error { _, err := nodeapp.ParseConfig(content); return err }},
		{"docker-install.json", docker, func(content []byte) error { _, err := dockerbootstrap.ParseManifest(content); return err }},
		{"computer-image.json", image, func(content []byte) error { _, err := dockernode.ParseImageManifest(content); return err }},
	} {
		content, err := json.MarshalIndent(item.body, "", "  ")
		if err != nil || item.validate(content) != nil {
			return configuration{}, nil, errors.New("生成的节点安装配置未通过校验")
		}
		files = append(files, configurationFile{name: item.name, content: append(content, '\n'), mode: 0600})
	}
	if err := value.validateNodeIdentity(platform); err != nil {
		return configuration{}, nil, err
	}
	paths := nodeInstallationPaths()
	references := []string{paths.Node, paths.Docker, paths.Image, filepath.Join(deploy.NodeDirectory, "installation.json")}
	for _, reference := range value.nodeReferences() {
		references = append(references, reference.path)
	}
	if err := validateFileLayout(references, value.nodeDirectories(), image.Socket); err != nil {
		return configuration{}, nil, err
	}
	if image.AuthenticationFile != "" && within(image.AuthenticationFile, deploy.NodeDirectory) {
		return configuration{}, nil, errors.New("仓库认证必须引用安装目录外的私有文件")
	}
	return value, files, nil
}

func createNodeCertificate(nodeID string, now time.Time) ([]configurationFile, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("生成节点证书身份失败")
	}
	defer clear(private)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, errors.New("生成节点证书序列失败")
	}
	serial.Add(serial, big.NewInt(1))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(nodeCertificateLifetime), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, errors.New("签发节点证书失败")
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, errors.New("编码节点私钥失败")
	}
	defer clear(key)
	return []configurationFile{{name: "certificate.pem", content: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), mode: 0600}, {name: "private.key", content: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), mode: 0600}}, nil
}

func installationReport(node nodeapp.Config, image dockernode.ImageManifest, certificate, key []byte) (NodeInstallationReport, error) {
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return NodeInstallationReport{}, errors.New("已安装节点的证书与私钥不匹配")
	}
	fingerprints, err := nodeapi.ParseFingerprints(node.Control.ClientFingerprints)
	if err != nil {
		return NodeInstallationReport{}, err
	}
	if _, err := nodeapi.ServerTLS(pair, fingerprints); err != nil {
		return NodeInstallationReport{}, err
	}
	fingerprint, err := nodeapi.FingerprintOf(pair)
	if err != nil {
		return NodeInstallationReport{}, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return NodeInstallationReport{}, errors.New("读取节点证书有效期失败")
	}
	return NodeInstallationReport{Scope: "node_installation", NodeID: node.NodeID, ImageID: image.ImageID, Listen: node.Listen, CertificateFingerprint: fingerprint, CertificateExpiresAt: leaf.NotAfter}, nil
}
