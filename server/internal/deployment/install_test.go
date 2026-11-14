package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/deploy"
)

func nodeInstallFixture(t *testing.T) (nodeInstaller, map[string]any, *[]string) {
	t.Helper()
	full, _ := preparationFixture(t)
	request := map[string]any{"version": 1, "control": map[string]any{"listen": "10.0.0.5:9443", "allow_public_listen": false, "client_fingerprints": []string{strings.Repeat("b", 64)}}, "image": full["image"], "docker": full["docker"]}
	fs := filesystem{base: t.TempDir(), owner: uint32(os.Geteuid())}
	for _, path := range []string{"/run", "/source"} {
		if err := os.Mkdir(fs.resolve(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	program := make([]byte, 64)
	copy(program, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(program[16:18], 2)
	binary.LittleEndian.PutUint16(program[18:20], 183)
	writeInstallFixture(t, fs, "/source/tomnode", program, 0755)
	writeInstallFixture(t, fs, "/source/node-install.json", encodeFixture(t, request), 0600)
	var calls []string
	installer := nodeInstaller{fs: fs, platform: "linux/arm64", caller: fs.owner, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), systemctl: func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}}
	return installer, request, &calls
}

func writeInstallFixture(t *testing.T, fs filesystem, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fs.resolve(path)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fs.resolve(path), content, mode); err != nil {
		t.Fatal(err)
	}
}

func installFixture(t *testing.T, installer nodeInstaller) NodeInstallationReport {
	t.Helper()
	report, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode")
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestNodeInstallationPublishesCompleteIdentityAndRetries(t *testing.T) {
	installer, request, calls := nodeInstallFixture(t)
	report := installFixture(t, installer)
	if report.Scope != "node_installation" || report.State != "control_started" || report.NodeID == "" || report.ImageID == "" || len(report.CertificateFingerprint) != 64 || time.Until(report.CertificateExpiresAt) < 364*24*time.Hour {
		t.Fatal("安装未返回可登记的节点身份")
	}
	if !reflect.DeepEqual(*calls, []string{"daemon-reload", "enable --now tombot-node.service", "is-active --quiet tombot-node.service"}) {
		t.Fatal("未等待系统服务真实启动", *calls)
	}
	contents := map[string][]byte{}
	for _, path := range []string{nodeBinaryPath, nodeServicePath, deploy.NodeDirectory + "/node.json", deploy.NodeDirectory + "/docker-install.json", deploy.NodeDirectory + "/computer-image.json", deploy.NodeDirectory + "/certificate.pem", deploy.NodeDirectory + "/private.key", deploy.NodeDirectory + "/installation.json"} {
		body, err := os.ReadFile(installer.fs.resolve(path))
		if err != nil {
			t.Fatal(err)
		}
		contents[path] = body
		mode := os.FileMode(0600)
		if path == nodeBinaryPath {
			mode = 0755
		} else if path == nodeServicePath {
			mode = 0644
		}
		info, err := os.Stat(installer.fs.resolve(path))
		if err != nil || info.Mode() != mode {
			t.Fatal("安装权限不符", path, err)
		}
	}
	// 参数排版变化不应触发新身份，也不要求重复发布已有文件。
	reformatted, err := json.MarshalIndent(request, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, installer.fs, "/source/node-install.json", reformatted, 0600)
	if repeated := installFixture(t, installer); repeated != report {
		t.Fatal("重复安装改变节点身份")
	}
	for path, expected := range contents {
		actual, err := os.ReadFile(installer.fs.resolve(path))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("重复安装修改已发布文件", path, err)
		}
	}
	for _, parent := range []string{"/etc/tombot", "/usr/lib/tombot", "/etc/systemd/system"} {
		entries, err := os.ReadDir(installer.fs.resolve(parent))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				t.Fatal("成功安装遗留暂存目录")
			}
		}
	}
}

func TestNodeInstallationFailureKeepsIdentityForRetry(t *testing.T) {
	for _, failure := range []string{"daemon-reload", "enable", "is-active", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			installer, _, calls := nodeInstallFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			systemctl := installer.systemctl
			installer.systemctl = func(ctx context.Context, args ...string) error {
				if err := systemctl(ctx, args...); err != nil {
					return err
				}
				if failure == "cancel" {
					cancel()
				} else if args[0] == failure {
					return errors.New("模拟系统服务失败")
				}
				return nil
			}
			report, err := installer.install(ctx, "/source/node-install.json", "/source/tomnode")
			if err == nil || report.State != "" {
				t.Fatal("未启动的服务被报告为成功")
			}
			wantCalls := map[string]int{"daemon-reload": 1, "enable": 2, "is-active": 3, "cancel": 1}[failure]
			if len(*calls) != wantCalls {
				t.Fatal("失败或取消后仍进行后续操作")
			}
			certificate, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/certificate.pem"))
			if err != nil {
				t.Fatal(err)
			}
			// 模拟配置已发布、程序或服务尚未发布的中断点。
			for _, path := range []string{nodeBinaryPath, nodeServicePath} {
				if err := os.Remove(installer.fs.resolve(path)); err != nil {
					t.Fatal(err)
				}
			}
			installer.systemctl = systemctl
			installFixture(t, installer)
			repeated, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/certificate.pem"))
			if err != nil || !bytes.Equal(certificate, repeated) {
				t.Fatal("失败重试重新签发了节点证书", err)
			}
		})
	}
}

func TestNodeInstallationRejectsConflictingExistingState(t *testing.T) {
	for _, failure := range []string{"request", "binary", "node.json", "private.key", "installation.json", "permissions", "unit"} {
		t.Run(failure, func(t *testing.T) {
			installer, request, calls := nodeInstallFixture(t)
			installFixture(t, installer)
			*calls = nil
			switch failure {
			case "request":
				request["control"].(map[string]any)["listen"] = "10.0.0.6:9443"
				writeInstallFixture(t, installer.fs, "/source/node-install.json", encodeFixture(t, request), 0600)
			case "binary":
				program, err := os.ReadFile(installer.fs.resolve("/source/tomnode"))
				if err != nil {
					t.Fatal(err)
				}
				writeInstallFixture(t, installer.fs, "/source/tomnode", append(program, 1), 0755)
			case "permissions":
				if err := os.Chmod(installer.fs.resolve(deploy.NodeDirectory+"/node.json"), 0644); err != nil {
					t.Fatal(err)
				}
			case "unit":
				writeInstallFixture(t, installer.fs, nodeServicePath, []byte("modified"), 0644)
			default:
				if err := os.Remove(installer.fs.resolve(deploy.NodeDirectory + "/" + failure)); err != nil {
					t.Fatal(err)
				}
			}
			if report, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode"); err == nil || report.State != "" || len(*calls) != 0 {
				t.Fatal("冲突安装覆盖了状态或控制服务", err)
			}
		})
	}
}

func TestNodeInstallationRejectsUnsafeInputBeforePublishing(t *testing.T) {
	for _, failure := range []string{"cancel", "writable", "symlink", "hardlink", "parent", "architecture", "binary-conflict", "unit-conflict", "directory-conflict", "locked"} {
		t.Run(failure, func(t *testing.T) {
			installer, _, calls := nodeInstallFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requestPath := "/source/node-install.json"
			switch failure {
			case "cancel":
				cancel()
			case "writable", "parent":
				path := requestPath
				if failure == "parent" {
					path = "/source"
				}
				if err := os.Chmod(installer.fs.resolve(path), 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink", "hardlink":
				link := os.Link
				if failure == "symlink" {
					link = os.Symlink
				}
				if err := link(installer.fs.resolve(requestPath), installer.fs.resolve("/source/linked.json")); err != nil {
					t.Fatal(err)
				}
				requestPath = "/source/linked.json"
			case "architecture":
				installer.platform = "linux/amd64"
			case "binary-conflict":
				writeInstallFixture(t, installer.fs, nodeBinaryPath, []byte("previous"), 0755)
			case "unit-conflict":
				writeInstallFixture(t, installer.fs, nodeServicePath, []byte("previous"), 0644)
			case "directory-conflict":
				if err := os.MkdirAll(installer.fs.resolve(deploy.NodeDirectory), 0700); err != nil {
					t.Fatal(err)
				}
			case "locked":
				unlock, err := installer.fs.lockNodeInstallation()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := unlock(); err != nil {
						t.Error(err)
					}
				})
			}
			if report, err := installer.install(ctx, requestPath, "/source/tomnode"); err == nil || report.State != "" || len(*calls) != 0 {
				t.Fatal("不安全安装输入被接受", err)
			}
			if _, err := os.Lstat(installer.fs.resolve(deploy.NodeDirectory + "/installation.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("失败安装发布了节点身份", err)
			}
		})
	}
}

func TestNodeInstallParametersRejectInvalidConfiguration(t *testing.T) {
	for _, failure := range []string{"version", "unknown", "fingerprint", "listen", "image", "docker", "node-id", "override", "path-overlap", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			_, request, _ := nodeInstallFixture(t)
			switch failure {
			case "version":
				request["version"] = 2
			case "unknown":
				request["unrecognized"] = true
			case "fingerprint":
				request["control"].(map[string]any)["client_fingerprints"] = []string{}
			case "listen":
				request["control"].(map[string]any)["listen"] = "0.0.0.0:9443"
			case "image":
				request["image"].(map[string]any)["reference"] = "example.test/computer:latest"
			case "docker":
				request["docker"] = map[string]any{}
			case "node-id":
				request["node_id"] = "invalid"
			case "override":
				request["node"] = map[string]any{"control_tls": map[string]any{"certificate_file": "/outside.pem"}}
			case "path-overlap":
				request["node"] = map[string]any{"runtime": map[string]any{"state_directory": deploy.NodeDirectory}}
			case "timeout":
				request["image"].(map[string]any)["timeout_seconds"] = 0
			}
			parsed, _, err := parseNodeInstallRequest(encodeFixture(t, request))
			if err == nil {
				_, _, err = renderNodeInstallation(parsed, "linux/arm64")
			}
			if err == nil {
				t.Fatal("错误安装配置被接受", failure)
			}
		})
	}
}

func TestNodeCertificateRejectsExpiredOrMismatchedIdentity(t *testing.T) {
	_, request, _ := nodeInstallFixture(t)
	parsed, _, err := parseNodeInstallRequest(encodeFixture(t, request))
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := renderNodeInstallation(parsed, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	current, err := createNodeCertificate(value.node.NodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(current[1].content)
	expired, err := createNodeCertificate(value.node.NodeID, time.Now().Add(-2*nodeCertificateLifetime))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(expired[1].content)
	for _, pair := range [][2][]byte{{expired[0].content, expired[1].content}, {current[0].content, expired[1].content}} {
		if _, err := installationReport(value.node, value.image, pair[0], pair[1]); err == nil {
			t.Fatal("过期或不匹配的节点身份被接受")
		}
	}
}
