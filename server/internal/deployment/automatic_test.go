package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/nodeenrollment"
)

func automaticFixture(t *testing.T) (*nodeInstaller, *int, *int) {
	t.Helper()
	installer, request, _ := nodeInstallFixture(t)
	archive := []byte("仅供摘要与原子发布测试的归档内容")
	digest := sha256.Sum256(archive)
	release := nodeRelease{Version: 1, Platform: installer.platform, ImageReference: request["image"].(map[string]any)["reference"].(string), ArchiveSHA256: hex.EncodeToString(digest[:]), Docker: encodeFixture(t, request["docker"])}
	encoded, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, installer.fs, "/source/node-release.json", encoded, 0644)
	writeInstallFixture(t, installer.fs, "/source/computer-image.tar", archive, 0644)
	installer.detectHost = func(context.Context) (installationHost, error) {
		return installationHost{Hostname: "test-node", Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.5")}, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 100 << 30}, nil
	}
	reads, fetches := 0, 0
	installer.fetchBootstrap = func(context.Context, nodeenrollment.JoinCode) (nodeenrollment.BootstrapConfiguration, error) {
		fetches++
		return nodeenrollment.BootstrapConfiguration{ClientFingerprints: []string{strings.Repeat("b", 64)}, VerificationResources: computer.Resources{CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 1 << 30, ProcessLimit: 512}}, nil
	}
	installer.prepareInstallation = func(ctx context.Context) (installationInput, error) {
		return installer.prepareAutomatic(ctx, "", "/source/tomnode", func(context.Context) ([]byte, error) {
			reads++
			raw := generatedEnrollmentToken()
			defer clear(raw)
			code, err := nodeenrollment.EncodeJoinCode(nodeenrollment.JoinCode{URL: "https://api.example.test", Fingerprints: []string{strings.Repeat("c", 64)}, Token: string(raw)})
			return []byte(code), err
		}, func(context.Context, string) (string, error) { return "", nil })
	}
	return &installer, &reads, &fetches
}
func TestAutomaticInstallWithoutParameterFile(t *testing.T) {
	installer, reads, fetches := automaticFixture(t)
	if err := os.Remove(installer.fs.resolve("/source/node-install.json")); err != nil {
		t.Fatal(err)
	}
	report, err := installer.install(t.Context(), "", "/source/tomnode")
	if err != nil {
		t.Fatal(err)
	}
	if *reads != 1 || *fetches != 1 {
		t.Fatal("首次安装未正确获取接入配置")
	}
	saved, err := os.ReadFile(installer.fs.resolve(filepath.Join(deploy.NodeDirectory, "installation.json")))
	if err != nil {
		t.Fatal(err)
	}
	var record nodeInstallation
	if json.Unmarshal(saved, &record) != nil {
		t.Fatal("安装快照无效")
	}
	request, _, err := parseNodeInstallRequest(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	if request.Capacity != 6 || request.Control.Listen != "10.0.0.5:9444" || request.Server.NodeURL != "https://10.0.0.5:9444" || !strings.HasPrefix(request.Name, "test-node-") {
		t.Fatal("自动资源和地址计算不符")
	}
	if bytes.Contains(saved, []byte("tomnode1.")) {
		t.Fatal("接入码进入公开配置")
	}
	for _, name := range []string{"node-release.json", "computer-image.tar"} {
		if err := os.Remove(installer.fs.resolve("/source/" + name)); err != nil {
			t.Fatal(err)
		}
	}
	again, err := installer.install(t.Context(), "", "/source/tomnode")
	if err != nil || again.NodeID != report.NodeID || *reads != 1 || *fetches != 1 {
		t.Fatal("重试依赖原安装包、重复询问或重新生成身份", err)
	}
}
func TestAutomaticInstallRejectsChangedArchive(t *testing.T) {
	installer, _, _ := automaticFixture(t)
	writeInstallFixture(t, installer.fs, "/source/computer-image.tar", []byte("损坏内容"), 0644)
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err == nil {
		t.Fatal("损坏离线归档被发布")
	}
	if _, err := os.Stat(installer.fs.resolve(deploy.NodeDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("错误后仍留下已发布身份")
	}
}
func TestAutomaticInstallRejectsUnavailableBootstrapAndInsufficientResources(t *testing.T) {
	for _, kind := range []string{"授权失败", "资源不足", "错误架构"} {
		t.Run(kind, func(t *testing.T) {
			installer, _, _ := automaticFixture(t)
			switch kind {
			case "授权失败":
				installer.fetchBootstrap = func(context.Context, nodeenrollment.JoinCode) (nodeenrollment.BootstrapConfiguration, error) {
					return nodeenrollment.BootstrapConfiguration{}, nodeenrollment.ErrUnauthorized
				}
			case "资源不足":
				installer.detectHost = func(context.Context) (installationHost, error) {
					return installationHost{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30}, nil
				}
			case "错误架构":
				installer.platform = "linux/amd64"
			}
			if _, err := installer.install(t.Context(), "", "/source/tomnode"); err == nil {
				t.Fatal("不可安装状态仍成功")
			}
			if _, err := os.Stat(installer.fs.resolve(deploy.NodeDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("失败仍发布配置")
			}
		})
	}
}

func TestAutomaticInstallProtectsSavedArchive(t *testing.T) {
	installer, _, _ := automaticFixture(t)
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err != nil {
		t.Fatal(err)
	}
	archive := installer.fs.resolve(filepath.Join(deploy.NodeDirectory, "computer-image.tar"))
	if err := os.WriteFile(archive, []byte("安装后损坏"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err == nil {
		t.Fatal("重试忽略了已保存归档损坏")
	}
}

func TestAutomaticOptionsAndAddressConfirmation(t *testing.T) {
	for _, kind := range []string{"覆盖", "取消", "非本机监听", "非法地址", "容量过大", "多网卡"} {
		t.Run(kind, func(t *testing.T) {
			installer, _, _ := automaticFixture(t)
			options := NodeInstallOptions{Name: "指定节点", Capacity: 2, Listen: "10.0.0.5:9444", URL: "https://node.example.test:9444"}
			switch kind {
			case "取消":
				options.URL = ""
			case "非本机监听":
				options.Listen = "10.0.0.99:9443"
			case "非法地址":
				options.URL = "http://node.example.test"
			case "容量过大":
				options.Capacity = 100
			case "多网卡":
				options.Listen = ""
				detect := installer.detectHost
				installer.detectHost = func(ctx context.Context) (installationHost, error) {
					host, err := detect(ctx)
					host.Addresses = append(host.Addresses, netip.MustParseAddr("10.0.0.6"))
					return host, err
				}
			}
			writeInstallFixture(t, installer.fs, "/source/options.json", encodeFixture(t, options), 0600)
			input, err := installer.prepareAutomatic(t.Context(), "/source/options.json", "/source/tomnode", func(context.Context) ([]byte, error) {
				token := generatedEnrollmentToken()
				defer clear(token)
				code, err := nodeenrollment.EncodeJoinCode(nodeenrollment.JoinCode{URL: "https://api.example.test", Fingerprints: []string{strings.Repeat("c", 64)}, Token: string(token)})
				return []byte(code), err
			}, func(context.Context, string) (string, error) {
				if kind == "多网卡" {
					return "10.0.0.6:9443", nil
				}
				return "", context.Canceled
			})
			defer clear(input.token)
			if kind == "覆盖" || kind == "多网卡" {
				if err != nil || input.request.Name != options.Name || input.request.Capacity != 2 || input.request.Server.NodeURL != options.URL {
					t.Fatal("可选覆盖未正确生成", err)
				}
				if kind == "多网卡" && input.request.Control.Listen != "10.0.0.6:9443" {
					t.Fatal("多网卡未采用显式选择")
				}
			} else if err == nil {
				t.Fatal("错误参数或取消仍生成配置")
			}
		})
	}
}

func TestOfflineArchiveCannotOverlapNodeFiles(t *testing.T) {
	installer, _, _ := automaticFixture(t)
	input, err := installer.prepareInstallation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(input.token)
	input.request.Image.ArchiveFile = filepath.Join(deploy.NodeDirectory, "certificate.pem")
	if _, _, err := renderNodeInstallation(input.request, installer.platform); err == nil {
		t.Fatal("离线归档允许覆盖节点证书")
	}
}
