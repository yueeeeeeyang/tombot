package deployment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
)

func nodeFilesystemFixture(t *testing.T) (filesystem, configuration, Paths) {
	t.Helper()
	fs, value, paths, _ := filesystemFixture(t)
	// 独立节点不部署主服务文件；直接移除夹具引用，防止节点检查意外依赖它们。
	for _, rule := range value.references() {
		if rule.api {
			if err := os.Remove(fs.resolve(rule.path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, path := range []string{paths.Server} {
		if err := os.Remove(fs.resolve(path)); err != nil {
			t.Fatal(err)
		}
	}
	paths.Server = ""
	return fs, value, paths
}

func TestNodeCheckWithoutAPIResources(t *testing.T) {
	fs, value, paths := nodeFilesystemFixture(t)
	report, err := checkNode(t.Context(), paths, "linux/arm64", fs)
	if err != nil || report.Scope != "node_configuration" || report.NodeID != value.node.NodeID || report.ImageID != value.image.ImageID || report.APIUID != 0 {
		t.Fatal("独立节点检查依赖 API 资源", report, err)
	}
	for _, path := range value.nodeDirectories() {
		if _, err := os.Stat(fs.resolve(path)); !os.IsNotExist(err) {
			t.Fatal("只读检查创建了状态目录", path, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if report, err := checkNode(ctx, paths, "linux/arm64", fs); err == nil || report.Scope != "" {
		t.Fatal("取消后输出成功")
	}
}

func TestNodeCheckRejectsInvalidConfigurationAndFiles(t *testing.T) {
	for _, kind := range []string{"node", "docker", "image", "identity", "platform", "socket", "directory-overlap", "file-overlap", "missing-key", "public-key", "public-config", "symlink", "hardlink", "wide-parent", "state-link", "missing-authentication"} {
		t.Run(kind, func(t *testing.T) {
			fs, value, paths := nodeFilesystemFixture(t)
			var err error
			switch kind {
			case "node", "docker", "image":
				path := map[string]string{"node": paths.Node, "docker": paths.Docker, "image": paths.Image}[kind]
				err = os.WriteFile(fs.resolve(path), []byte(`{"version":999}`), 0600)
			case "identity":
				value.image.NodeID = ids.New()
			case "platform":
				value.image.Platform = "linux/amd64"
			case "socket":
				value.image.Socket = "/run/other.sock"
			case "directory-overlap":
				value.image.StateDirectory = value.node.Runtime.StateDirectory + "/images"
			case "file-overlap":
				value.image.AuthenticationFile = paths.Node
			case "missing-authentication":
				value.image.AuthenticationFile = "/etc/tombot/private/registry.json"
			case "missing-key":
				err = os.Remove(fs.resolve(value.node.Control.PrivateKeyFile))
			case "public-key":
				err = os.Chmod(fs.resolve(value.node.Control.PrivateKeyFile), 0644)
			case "public-config":
				err = os.Chmod(fs.resolve(paths.Node), 0644)
			case "symlink":
				path := fs.resolve(value.node.Control.PrivateKeyFile)
				if err = os.Rename(path, path+".original"); err == nil {
					err = os.Symlink(path+".original", path)
				}
			case "hardlink":
				path := fs.resolve(value.node.Control.PrivateKeyFile)
				err = os.Link(path, path+".alias")
			case "wide-parent":
				err = os.Chmod(fs.resolve(filepath.Dir(paths.Node)), 0777)
			case "state-link":
				if err = os.MkdirAll(fs.resolve(filepath.Dir(value.image.StateDirectory)), 0755); err == nil {
					err = os.Symlink(fs.resolve("/etc"), fs.resolve(value.image.StateDirectory))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(kind, "overlap") || kind == "identity" || kind == "platform" || kind == "socket" || kind == "missing-authentication" {
				if err := os.WriteFile(fs.resolve(paths.Image), encodeFixture(t, value.image), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if report, err := checkNode(t.Context(), paths, "linux/arm64", fs); err == nil || report.Scope != "" {
				t.Fatal("无效节点配置产生成功回执", report)
			}
		})
	}
}
