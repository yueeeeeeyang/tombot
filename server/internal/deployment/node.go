package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
)

// CheckNode 在 Linux root 下核对节点的三份配置及引用权限；不读取 API 配置或要求 API 系统账户。
// 成功仅证明部署配置可用，宿主安装、TLS 握手和电脑能力仍由各自实际运行步骤验证。
func CheckNode(ctx context.Context, paths Paths) (Report, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return Report{}, errors.New("节点部署检查必须由 Linux root 执行")
	}
	return checkNode(ctx, paths, "linux/"+runtime.GOARCH, filesystem{base: "/", owner: 0})
}

func (value configuration) nodeDirectories() []string {
	return []string{value.node.Runtime.StateDirectory, value.node.Runtime.StorageDirectory, value.node.Storage.StateDirectory, value.image.StateDirectory, dockerbootstrap.StateDirectory, dockerbootstrap.RuntimeDirectory}
}

func (value configuration) nodeReferences() []fileRule {
	files := []fileRule{
		{path: value.node.Control.CertificateFile, maximum: 1 << 20},
		{path: value.node.Control.PrivateKeyFile, maximum: 64 << 10, private: true},
	}
	if value.node.Enrollment != nil {
		files = append(files, fileRule{path: value.node.Enrollment.Server.TokenFile, maximum: 128, private: true})
	}
	if value.image.AuthenticationFile != "" {
		files = append(files, fileRule{path: value.image.AuthenticationFile, maximum: 16 << 10, private: true})
	}
	if value.image.ArchiveFile != "" {
		files = append(files, fileRule{path: value.image.ArchiveFile, maximum: dockernode.MaximumImageArchive, private: true})
	}
	return files
}

func (value configuration) validateNodeIdentity(platform string) error {
	if value.node.Enrollment != nil {
		r := value.node.Enrollment.Registration
		if r.ImageID != value.image.ImageID || r.ImageReference != value.image.Reference {
			return errors.New("自动接入与准备镜像身份不一致")
		}
	}
	if value.node.NodeID != value.image.NodeID {
		return errors.New("节点与镜像清单身份不一致")
	}
	if value.node.Runtime.Socket != value.image.Socket || value.image.Socket != dockerbootstrap.SocketPath {
		return errors.New("节点、镜像与 Docker 引导的控制端点不一致")
	}
	if value.image.Platform != platform {
		return errors.New("镜像平台与当前宿主架构不一致")
	}
	return nil
}

func checkNode(ctx context.Context, paths Paths, platform string, fs filesystem) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	var value configuration
	loaders := []struct {
		name    string
		path    string
		maximum int64
		parse   func([]byte) error
	}{
		{"节点", paths.Node, 16 << 10, func(encoded []byte) (err error) { value.node, err = nodeapp.ParseConfig(encoded); return err }},
		{"Docker 引导", paths.Docker, 64 << 10, func(encoded []byte) error { _, err := dockerbootstrap.ParseManifest(encoded); return err }},
		{"镜像", paths.Image, 16 << 10, func(encoded []byte) (err error) {
			value.image, err = dockernode.ParseImageManifest(encoded)
			return err
		}},
	}
	for _, loader := range loaders {
		encoded, err := fs.readConfig(fileRule{path: loader.path, maximum: loader.maximum, private: true}, account{})
		if err != nil {
			return Report{}, fmt.Errorf("%s配置检查失败：%w", loader.name, err)
		}
		if loader.parse(encoded) != nil {
			return Report{}, fmt.Errorf("%s配置未通过所属组件校验", loader.name)
		}
	}
	if err := value.validateNodeIdentity(platform); err != nil {
		return Report{}, err
	}
	files := []string{paths.Node, paths.Docker, paths.Image}
	for _, rule := range value.nodeReferences() {
		files = append(files, rule.path)
	}
	if err := validateFileLayout(files, value.nodeDirectories(), value.image.Socket); err != nil {
		return Report{}, err
	}
	for _, rule := range value.nodeReferences() {
		if _, err := fs.inspect(rule, account{}); err != nil {
			return Report{}, err
		}
	}
	for _, directory := range value.nodeDirectories() {
		if err := fs.directory(directory, account{}, false, false, true); err != nil {
			return Report{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return Report{Scope: "node_configuration", NodeID: value.node.NodeID, ImageID: value.image.ImageID, Platform: platform}, nil
}
