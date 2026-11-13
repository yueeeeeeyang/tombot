package deployment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/serverapp"
	"io.yueyang/tom/internal/store/postgres"
)

func fixtureConfiguration(t *testing.T) (configuration, Paths, account) {
	t.Helper()
	encoded, err := os.ReadFile("../../deploy/node.example.json")
	if err != nil {
		t.Fatal(err)
	}
	node, err := nodeapp.ParseConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	server := serverapp.DefaultConfig()
	server.Address, server.AdminOrigin = "127.0.0.1:8443", "https://127.0.0.1:8443"
	server.AdminDirectory = "/opt/tombot/admin"
	server.TLSCertificateFile, server.TLSPrivateKeyFile = "/etc/tombot/certificate.pem", "/etc/tombot/private/tls.pem"
	server.IdentityKeyFile, server.CSRFKeyFile = "/etc/tombot/private/identity.key", "/etc/tombot/private/csrf.key"
	server.ActiveModelKey = "current"
	server.ModelKeyFiles = map[string]string{"current": "/etc/tombot/private/model.key"}
	server.Database.DSNFiles = make(map[postgres.Role]string)
	for _, role := range []postgres.Role{postgres.IdentityRole, postgres.ControlRole, postgres.TenantRole, postgres.DispatchRole} {
		server.Database.DSNFiles[role] = "/etc/tombot/private/" + string(role) + ".connection"
	}
	server.Node.ID, server.Node.Verification.ImageID = node.NodeID, ids.New()
	server.Node.URL = "https://" + node.Listen
	server.Node.CertificateFile, server.Node.PrivateKeyFile = "/etc/tombot/tls/api-certificate.pem", "/etc/tombot/private/api.key"
	server.Node.ServerFingerprints = []string{strings.Repeat("b", 64)}
	server.Models.Origins = []serverapp.Origin{{URL: "https://model.example.test"}}
	image := dockernode.ImageManifest{Version: 1, NodeID: node.NodeID, ImageID: server.Node.Verification.ImageID, Reference: "example.test/computer@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", Socket: dockerbootstrap.SocketPath, StateDirectory: "/var/lib/tombot-node/images", CallTimeoutSeconds: 30, TimeoutSeconds: 60}
	paths := Paths{Server: "/etc/tombot/server.json", Node: "/etc/tombot/node.json", Docker: "/etc/tombot/docker-install.json", Image: "/etc/tombot/computer-image.json"}
	return configuration{server: server, node: node, image: image}, paths, account{uid: 20001, gid: 20001}
}

func TestDeploymentGraph(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*configuration, *Paths, *account)
		want   string
	}{
		{"部署示例与服务默认值", func(*configuration, *Paths, *account) {}, ""},
		{"旧示例拒绝三分钟验收", func(c *configuration, _ *Paths, _ *account) { c.node.Runtime.MaximumLease = 120000 }, "租约"},
		{"租约恰好足够", func(c *configuration, _ *Paths, _ *account) { c.node.Runtime.MaximumLease = 180000 }, ""},
		{"开户租约越界", func(c *configuration, _ *Paths, _ *account) {
			c.server.Provision.LeaseDuration = serverapp.Duration(10 * time.Minute)
		}, "租约"},
		{"节点身份", func(c *configuration, _ *Paths, _ *account) { c.node.NodeID = ids.New() }, "身份"},
		{"镜像身份", func(c *configuration, _ *Paths, _ *account) { c.image.ImageID = ids.New() }, "身份"},
		{"用户身份", func(_ *configuration, _ *Paths, a *account) { a.uid = 0 }, "账户"},
		{"root 身份", func(_ *configuration, _ *Paths, a *account) { a.uid = 0 }, "账户"},
		{"节点端口不一致", func(c *configuration, _ *Paths, _ *account) { c.server.Node.URL = "https://10.0.0.5:9444" }, "端口"},
		{"状态目录包含节点证书", func(c *configuration, _ *Paths, _ *account) {
			c.node.Runtime.StateDirectory = "/etc/tombot/private"
		}, "重叠"},
		{"引导引擎", func(c *configuration, _ *Paths, _ *account) {
			c.node.Runtime.Socket, c.image.Socket = "/run/other.sock", "/run/other.sock"
		}, "端点"},
		{"架构", func(c *configuration, _ *Paths, _ *account) { c.image.Platform = "linux/amd64" }, "架构"},
		{"调用预算", func(c *configuration, _ *Paths, _ *account) { c.node.API.CallTimeout = 10000 }, "预算"},
		{"桌面额度", func(c *configuration, _ *Paths, _ *account) {
			c.server.Provision.DesktopConcurrency = c.node.Agent.MaximumActive + 1
		}, "并发"},
		{"镜像记录嵌套", func(c *configuration, _ *Paths, _ *account) {
			c.image.StateDirectory = c.node.Runtime.StateDirectory + "/images"
		}, "重叠"},
		{"引导记录被包含", func(c *configuration, _ *Paths, _ *account) { c.image.StateDirectory = "/var/lib/tombot-node" }, "重叠"},

		{"管理端包含私有文件", func(c *configuration, _ *Paths, _ *account) { c.server.AdminDirectory = "/etc/tombot" }, "重叠"},
		{"私有文件复用", func(c *configuration, _ *Paths, _ *account) { c.server.CSRFKeyFile = c.server.IdentityKeyFile }, "复用"},
		{"配置与镜像记录重叠", func(c *configuration, p *Paths, _ *account) { p.Image = c.image.StateDirectory + "/config.json" }, "重叠"},
		{"路径非规范", func(_ *configuration, p *Paths, _ *account) { p.Node = "/etc/tombot/../node.json" }, "路径"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			value, paths, api := fixtureConfiguration(t)
			fixture.change(&value, &paths, &api)
			err := value.validateGraph(paths, api, "linux/arm64")
			if fixture.want == "" && err != nil || fixture.want != "" && (err == nil || !strings.Contains(err.Error(), fixture.want)) {
				t.Fatalf("配置关系检查结果不符: %v", err)
			}
		})
	}
}

func writeFixture(t *testing.T, fs filesystem, path string, content []byte, mode os.FileMode, uid uint32) {
	t.Helper()
	path = fs.resolve(path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Geteuid()) {
		if err := os.Chown(path, int(uid), -1); err != nil {
			t.Fatal(err)
		}
	}
}

func encodeFixture(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// materializeFixture 中的内容只用于元信息检查，不是有效凭据或受测发布清单。
func materializeFixture(t *testing.T, fs filesystem, value configuration, paths Paths, api account) {
	t.Helper()
	manifest := dockerbootstrap.Manifest{EnginePackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", CLIPackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", ContainerdPackageVersion: "2.3.6-1~ubuntu.24.04~noble", E2fsprogsPackageVersion: "1.47.0-test1", UtilLinuxPackageVersion: "2.39.3-test1", MountPackageVersion: "2.39.3-test1", AllowedEngineVersions: []string{"29.7.2"}, RepositoryFingerprint: strings.Repeat("A", 40), TimeoutSeconds: 300, MinimumFreeBytes: 1 << 30}
	for _, fixture := range []struct {
		path string
		body any
		mode os.FileMode
	}{{paths.Server, value.server, 0644}, {paths.Node, value.node, 0600}, {paths.Image, value.image, 0600}, {paths.Docker, manifest, 0600}} {
		writeFixture(t, fs, fixture.path, encodeFixture(t, fixture.body), fixture.mode, fs.owner)
	}
	for _, rule := range value.references() {
		owner := fs.owner
		if rule.api {
			owner = api.uid
		}
		writeFixture(t, fs, rule.path, make([]byte, 32), 0600, owner)
	}
	if err := os.MkdirAll(fs.resolve(value.server.AdminDirectory), 0755); err != nil {
		t.Fatal(err)
	}
}

func filesystemFixture(t *testing.T) (filesystem, configuration, Paths, account) {
	t.Helper()
	fs := filesystem{base: t.TempDir(), owner: uint32(os.Geteuid())}
	if err := os.Chmod(fs.base, 0755); err != nil {
		t.Fatal(err)
	}
	value, paths, api := fixtureConfiguration(t)
	if fs.owner != 0 {
		api = account{uid: fs.owner, gid: uint32(os.Getegid())}
	}
	materializeFixture(t, fs, value, paths, api)
	return fs, value, paths, api
}

func TestCheckReadsConfigurationButOnlyInspectsReferences(t *testing.T) {
	fs, value, paths, api := filesystemFixture(t)
	report, err := check(t.Context(), paths, api, "linux/arm64", fs)
	if err != nil || report.Scope != "deployment_configuration" || report.NodeID != value.node.NodeID || report.APIUID != api.uid {
		t.Fatal("完整配置检查失败", report, err)
	}
	for _, path := range []string{value.node.Runtime.StateDirectory, value.node.Runtime.StorageDirectory, value.image.StateDirectory} {
		if _, err := os.Lstat(fs.resolve(path)); !os.IsNotExist(err) {
			t.Fatal("检查创建了部署状态", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if report, err := check(ctx, paths, api, "linux/arm64", fs); err == nil || report.Scope != "" {
		t.Fatal("取消产生成功回执")
	}
}

func TestCheckRejectsInvalidFiles(t *testing.T) {
	for _, kind := range []string{"server", "node", "docker", "image", "missing", "wide", "short", "link", "hardlink", "parent", "fifo", "unreadable", "private-parent", "state-link"} {
		t.Run(kind, func(t *testing.T) {
			fs, value, paths, api := filesystemFixture(t)
			path := fs.resolve(value.server.IdentityKeyFile)
			var err error
			switch kind {
			case "server", "node", "docker", "image":
				path = fs.resolve(map[string]string{"server": paths.Server, "node": paths.Node, "docker": paths.Docker, "image": paths.Image}[kind])
				err = os.WriteFile(path, []byte(`{"unknown":true}`), 0600)
			case "missing":
				err = os.Remove(path)
			case "wide":
				err = os.Chmod(path, 0644)
			case "short":
				err = os.WriteFile(path, make([]byte, 31), 0600)
			case "link", "fifo":
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "link" {
					err = os.Symlink(fs.resolve(value.server.CSRFKeyFile), path)
				} else {
					err = syscall.Mkfifo(path, 0600)
				}
			case "hardlink":
				err = os.Link(path, fs.resolve("/etc/tombot/private/alias"))
			case "parent":
				err = os.Chmod(filepath.Dir(path), 0777)
			case "unreadable":
				err = os.Chmod(path, 0000)
			case "private-parent":
				err = os.Chmod(filepath.Dir(path), 0600)
				t.Cleanup(func() {
					if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
						t.Error(err)
					}
				})
			case "state-link":
				if err = os.MkdirAll(fs.resolve("/var/lib/tombot-node"), 0755); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(fs.resolve("/etc"), fs.resolve(value.image.StateDirectory))
			}
			if err != nil {
				t.Fatal(err)
			}
			if report, err := check(t.Context(), paths, api, "linux/arm64", fs); err == nil || report.Scope != "" {
				t.Fatal("无效部署文件产生成功回执", report)
			}
		})
	}
}

func TestDeploymentSystemdOrder(t *testing.T) {
	for name, required := range map[string][]string{
		"tombot-docker-bootstrap.service": {"Requires=tombot-deployment-check.service", "After=network-online.target tombot-deployment-check.service"},
		"tombot-deployment-check.service": {"ExecStart=/usr/lib/tombot/tomdeploy-check", "User=root", "ProtectSystem=strict", "TimeoutStartSec=30s", "Environment=TOM_SERVER_CONFIG=/etc/tombot/server.json", "Environment=TOM_NODE_CONFIG=/etc/tombot/node.json", "Environment=TOM_DOCKER_INSTALL_MANIFEST=/etc/tombot/docker-install.json", "Environment=TOM_IMAGE_PREPARATION_MANIFEST=/etc/tombot/computer-image.json"},
	} {
		encoded, err := os.ReadFile("../../deploy/systemd/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range required {
			if !strings.Contains(string(encoded), expected+"\n") {
				t.Fatal("部署启动依赖丢失", name, expected)
			}
		}
		if strings.Contains(string(encoded), "RemainAfterExit=yes") {
			t.Fatal("配置检查结果不应跨引导缓存")
		}
	}
}
