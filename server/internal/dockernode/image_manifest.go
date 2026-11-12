package dockernode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
)

// ImageManifest 由 root 部署文件提供；认证仅引用私有文件，不读取用户 Docker 登录配置。
type ImageManifest struct {
	Version            int    `json:"version"`
	NodeID             string `json:"node_id"`
	ImageID            string `json:"image_id"`
	Reference          string `json:"reference"`
	Platform           string `json:"platform"`
	Socket             string `json:"docker_socket"`
	StateDirectory     string `json:"state_directory"`
	AuthenticationFile string `json:"authentication_file,omitempty"`
	CallTimeoutSeconds int    `json:"call_timeout_seconds"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
}

func (manifest ImageManifest) validate() error {
	for _, value := range []string{manifest.NodeID, manifest.ImageID} {
		id, err := ulid.ParseStrict(value)
		if err != nil || id.String() != value {
			return failure("image_manifest_identity_invalid")
		}
	}
	if manifest.Version != 1 || computer.ValidateImageReference(manifest.Reference) != nil || manifest.Platform != "linux/amd64" && manifest.Platform != "linux/arm64" || manifest.CallTimeoutSeconds < 1 || manifest.CallTimeoutSeconds > 300 || manifest.TimeoutSeconds < 30 || manifest.TimeoutSeconds > 3600 || manifest.TimeoutSeconds < manifest.CallTimeoutSeconds {
		return failure("image_manifest_invalid")
	}
	paths := []string{manifest.Socket, manifest.StateDirectory}
	if manifest.AuthenticationFile != "" {
		paths = append(paths, manifest.AuthenticationFile)
	}
	for _, path := range paths {
		if len(path) < 2 || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\n\r") {
			return failure("image_manifest_path_invalid")
		}
	}
	return nil
}

// decodeImageFields 允许部署文件缩进，拒绝重复键、别名、未知字段、空值及尾随内容。
func decodeImageFields(encoded []byte, fields map[string]any) error {
	if !utf8.Valid(encoded) {
		return failure("image_manifest_encoding_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return failure("image_manifest_format_invalid")
	}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] == nil {
			return failure("image_manifest_field_invalid")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, fields[name]) != nil {
			return failure("image_manifest_value_invalid")
		}
		delete(fields, name)
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return failure("image_manifest_trailing_content")
	}
	return nil
}

// ParseImageManifest 复用镜像准备的格式与身份约束，不读取认证材料或调用 Docker。
func ParseImageManifest(encoded []byte) (ImageManifest, error) {
	var manifest ImageManifest
	if len(encoded) == 0 || len(encoded) > 16<<10 {
		return manifest, failure("image_manifest_size_invalid")
	}
	err := decodeImageFields(encoded, map[string]any{"version": &manifest.Version, "node_id": &manifest.NodeID, "image_id": &manifest.ImageID, "reference": &manifest.Reference, "platform": &manifest.Platform, "docker_socket": &manifest.Socket, "state_directory": &manifest.StateDirectory, "authentication_file": &manifest.AuthenticationFile, "call_timeout_seconds": &manifest.CallTimeoutSeconds, "timeout_seconds": &manifest.TimeoutSeconds})
	if err != nil {
		return ImageManifest{}, err
	}
	if err := manifest.validate(); err != nil {
		return ImageManifest{}, err
	}
	return manifest, nil
}

// ReadDeploymentFile 复用节点私有普通文件规则，并检查全部父目录；返回内容只供本机部署使用。
func ReadDeploymentFile(path string) (encoded []byte, result error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, failure("deployment_configuration_path_invalid")
	}
	if err := checkProtectedParents(filepath.Dir(path)); err != nil {
		return nil, failure("deployment_configuration_parent_unsafe")
	}
	journal, err := openJournal(filepath.Dir(path), 0)
	if err != nil {
		return nil, failure("deployment_configuration_directory_unsafe")
	}
	defer func() {
		if journal.root.Close() != nil {
			result = errors.Join(result, failure("deployment_configuration_close_failed"))
		}
	}()
	encoded, err = journal.readFile(filepath.Base(path))
	if err != nil || len(encoded) == 0 {
		return nil, failure("deployment_configuration_unavailable")
	}
	return encoded, nil
}

// imageAuthentication 只接受固定仓库的密码或身份凭据，不保存、打印或向其他端点转发。
func imageAuthentication(encoded []byte, reference string) (string, error) {
	var authentication struct {
		Username      string `json:"username,omitempty"`
		Password      string `json:"password,omitempty"`
		ServerAddress string `json:"serveraddress"`
		IdentityToken string `json:"identitytoken,omitempty"`
	}
	if err := decodeImageFields(encoded, map[string]any{"username": &authentication.Username, "password": &authentication.Password, "serveraddress": &authentication.ServerAddress, "identitytoken": &authentication.IdentityToken}); err != nil {
		return "", failure("image_authentication_invalid")
	}
	registry := "index.docker.io"
	first, _, hasPath := strings.Cut(reference, "/")
	if hasPath && (strings.ContainsAny(first, ".:") || first == "localhost") {
		registry = first
	}
	password := authentication.Username != "" && authentication.Password != "" && authentication.IdentityToken == ""
	token := authentication.IdentityToken != "" && authentication.Username == "" && authentication.Password == ""
	if authentication.ServerAddress != registry || !password && !token {
		return "", failure("image_authentication_scope_invalid")
	}
	canonical, err := json.Marshal(authentication)
	if err != nil {
		return "", failure("image_authentication_encoding_failed")
	}
	defer clear(canonical)
	return base64.URLEncoding.EncodeToString(canonical), nil
}
