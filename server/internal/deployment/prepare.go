package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/serverapp"
	"io.yueyang/tom/internal/store/postgres"
)

// preparation 只保存部署参数与文件引用，不生成或读取凭据，不登记外部资源。
type preparation struct {
	Version  int             `json:"version"`
	NodeID   string          `json:"node_id"`
	Name     string          `json:"node_name"`
	Capacity int             `json:"computer_capacity"`
	ImageID  string          `json:"image_id"`
	Image    imageSettings   `json:"image"`
	Docker   json.RawMessage `json:"docker"`
	Server   json.RawMessage `json:"server"`
	Node     json.RawMessage `json:"node,omitempty"`
}

type imageSettings struct {
	Reference          string `json:"reference"`
	AuthenticationFile string `json:"authentication_file,omitempty"`
	CallTimeoutSeconds int    `json:"call_timeout_seconds"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
}

type configurationFile struct {
	name    string
	content []byte
	mode    os.FileMode
}

// Prepare 在 Linux root 下读取一份参数，在全新目录原子交付五份配置；不覆盖旧目录或启动服务。
func Prepare(ctx context.Context, source, destination string) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return Report{}, errors.New("部署配置生成必须由 Linux root 执行")
	}
	api, err := lookupAccount()
	if err != nil {
		return Report{}, err
	}
	fs := filesystem{base: "/", owner: 0}
	encoded, err := fs.readConfig(fileRule{path: source, maximum: 256 << 10, private: true}, api)
	if err != nil {
		return Report{}, err
	}
	files, report, err := renderConfiguration(encoded, api, "linux/"+runtime.GOARCH)
	if err != nil {
		return Report{}, err
	}
	if err := publishConfiguration(ctx, fs, destination, files); err != nil {
		return Report{}, err
	}
	return report, nil
}

// decodePreparation 拒绝重复字段、空值、未知字段和大小写别名，不把解码器原始内容带入错误。
func decodePreparation(encoded []byte, target any) error {
	if err := validatePreparationJSON(encoded, target); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("部署参数格式无效或包含未知字段")
	}
	return nil
}

func renderConfiguration(encoded []byte, api account, platform string) ([]configurationFile, Report, error) {
	request := preparation{Image: imageSettings{CallTimeoutSeconds: 30, TimeoutSeconds: 600}}
	if len(encoded) == 0 || len(encoded) > 256<<10 || decodePreparation(encoded, &request) != nil || request.Version != 1 {
		return nil, Report{}, errors.New("部署参数文件无效或版本不支持")
	}
	node, err := nodeapp.ParseConfig(deploy.NodeExample())
	if err != nil {
		return nil, Report{}, errors.New("随包节点配置基线无效")
	}
	node.NodeID, node.ClientUID, node.ClientGID = request.NodeID, api.uid, api.gid
	if len(request.Node) != 0 && decodePreparation(request.Node, &node) != nil {
		return nil, Report{}, errors.New("节点配置覆盖参数无效")
	}
	server := serverapp.DefaultConfig()
	server.AdminDirectory = "/opt/tombot/admin"
	server.TLSCertificateFile, server.TLSPrivateKeyFile = "/etc/tombot/tls/certificate.pem", "/etc/tombot/private/tls.key"
	server.IdentityKeyFile, server.CSRFKeyFile = "/etc/tombot/private/identity.key", "/etc/tombot/private/csrf.key"
	server.ActiveModelKey, server.ModelKeyFiles = "current", map[string]string{"current": "/etc/tombot/private/model.key"}
	server.Database.DSNFiles = map[postgres.Role]string{postgres.IdentityRole: "/etc/tombot/private/identity.connection", postgres.ControlRole: "/etc/tombot/private/control.connection", postgres.TenantRole: "/etc/tombot/private/tenant.connection", postgres.DispatchRole: "/etc/tombot/private/dispatch.connection"}
	server.Node.ID, server.Node.Socket, server.Node.Verification.ImageID = request.NodeID, node.Socket, request.ImageID
	// 显式提供映射时整体替换，不能让空映射静默继承旧密钥或连接引用。
	var overrides map[string]json.RawMessage
	if json.Unmarshal(request.Server, &overrides) != nil {
		return nil, Report{}, errors.New("API 配置参数无效")
	}
	if _, exists := overrides["model_key_files"]; exists {
		server.ModelKeyFiles = nil
	}
	if body, exists := overrides["database"]; exists {
		var database map[string]json.RawMessage
		if json.Unmarshal(body, &database) != nil {
			return nil, Report{}, errors.New("数据库配置参数无效")
		}
		if _, exists := database["dsn_files"]; exists {
			server.Database.DSNFiles = nil
		}
	}
	if decodePreparation(request.Server, &server) != nil {
		return nil, Report{}, errors.New("API 配置参数无效")
	}
	image := dockernode.ImageManifest{Version: 1, NodeID: request.NodeID, ImageID: request.ImageID, Reference: request.Image.Reference, Platform: platform, Socket: dockerbootstrap.SocketPath, StateDirectory: "/var/lib/tombot-node/images", AuthenticationFile: request.Image.AuthenticationFile, CallTimeoutSeconds: request.Image.CallTimeoutSeconds, TimeoutSeconds: request.Image.TimeoutSeconds}
	docker, err := dockerbootstrap.ParseManifest(request.Docker)
	if err != nil || validatePreparationJSON(request.Docker, &dockerbootstrap.Manifest{}) != nil {
		return nil, Report{}, errors.New("Docker 与宿主软件清单无效")
	}
	registration := nodecatalog.Registration{Name: request.Name, Capacity: request.Capacity, ImageID: request.ImageID, ImageReference: image.Reference}
	if registration.Validate() != nil || node.NodeID != request.NodeID || node.ClientUID != api.uid || node.ClientGID != api.gid {
		return nil, Report{}, errors.New("节点登记参数或配置身份冲突")
	}
	if request.Capacity >= node.Runtime.MaximumEgressComputers {
		return nil, Report{}, errors.New("节点出网电脑容量必须覆盖业务电脑并为独立验收保留一个名额")
	}
	var value configuration
	files := []configurationFile{}
	for _, item := range []struct {
		name  string
		body  any
		mode  os.FileMode
		parse func([]byte) error
	}{
		{"server.json", server, 0644, func(body []byte) (err error) { value.server, err = serverapp.ParseConfig(body); return err }},
		{"node.json", node, 0600, func(body []byte) (err error) { value.node, err = nodeapp.ParseConfig(body); return err }},
		{"computer-image.json", image, 0600, func(body []byte) (err error) { value.image, err = dockernode.ParseImageManifest(body); return err }},
		{"docker-install.json", docker, 0600, nil},
		{"node-registration.json", serverapp.CatalogRequest{Action: "register", Name: request.Name, Capacity: request.Capacity, ImageID: request.ImageID, ImageReference: image.Reference}, 0644, nil},
	} {
		content, err := json.MarshalIndent(item.body, "", "  ")
		if err != nil || item.parse != nil && item.parse(content) != nil {
			return nil, Report{}, fmt.Errorf("生成的%s未通过组件配置校验", item.name)
		}
		files = append(files, configurationFile{name: item.name, content: append(content, '\n'), mode: item.mode})
	}
	paths := Paths{Server: "/etc/tombot/server.json", Node: "/etc/tombot/node.json", Docker: "/etc/tombot/docker-install.json", Image: "/etc/tombot/computer-image.json"}
	if err := value.validateGraph(paths, api, platform); err != nil {
		return nil, Report{}, err
	}
	for _, path := range append(value.referencePaths(), value.directories()...) {
		if within("/etc/tombot/node-registration.json", path) || within(path, "/etc/tombot/node-registration.json") {
			return nil, Report{}, errors.New("节点登记文件与配置引用或目录冲突")
		}
	}
	return files, Report{Scope: "deployment_configuration_files", NodeID: request.NodeID, ImageID: request.ImageID, Platform: platform, APIUID: api.uid, APIGID: api.gid}, nil
}

// publishConfiguration 先在受保护父目录写完并刷盘，再一次性发布；目标已存在时始终拒绝。
func publishConfiguration(ctx context.Context, fs filesystem, destination string, files []configurationFile) (result error) {
	if !validPath(destination) || len(files) != 5 {
		return errors.New("配置输出目录或文件集合无效")
	}
	parent := filepath.Dir(destination)
	if err := fs.directory(parent, account{}, false, false, false); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(fs.resolve(parent), ".tombot-configuration-")
	if err != nil {
		return errors.New("无法建立配置暂存目录")
	}
	published := false
	defer func() {
		if !published && os.RemoveAll(stage) != nil {
			result = errors.Join(result, errors.New("配置暂存目录清理失败"))
		}
	}()
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeConfigurationFile(filepath.Join(stage, file.name), file.content, file.mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return errors.New("配置目录权限设置失败")
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishDirectory(stage, fs.resolve(destination)); err != nil {
		return errors.New("配置目录发布失败，目标可能已存在，禁止覆盖或自动重放")
	}
	published = true
	if err := syncDirectory(fs.resolve(parent)); err != nil {
		return errors.New("配置目录已发布但刷盘未确认，请保留原目录核验")
	}
	return nil
}

func writeConfigurationFile(path string, content []byte, mode os.FileMode) (result error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("配置文件创建失败")
	}
	defer func() {
		if file.Close() != nil {
			result = errors.Join(result, errors.New("配置文件关闭失败"))
		}
	}()
	if count, err := file.Write(content); err != nil || count != len(content) {
		return errors.New("配置文件未完整写入")
	}
	if file.Chmod(mode) != nil || file.Sync() != nil {
		return errors.New("配置文件权限设置或刷盘失败")
	}
	return nil
}

func syncDirectory(path string) (result error) {
	directory, err := os.Open(path)
	if err != nil {
		return errors.New("配置目录无法打开")
	}
	defer func() {
		if directory.Close() != nil {
			result = errors.Join(result, errors.New("配置目录关闭失败"))
		}
	}()
	if directory.Sync() != nil {
		return errors.New("配置目录刷盘失败")
	}
	return nil
}
