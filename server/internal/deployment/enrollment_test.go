package deployment

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/nodeapp"
)

func enrollmentInstallRequest(request map[string]any) map[string]any {
	request["node_name"] = "自动接入节点"
	request["computer_capacity"] = 4
	server := map[string]any{"url": "https://api.example.test", "node_url": "https://10.0.0.5:9443", "certificate_fingerprints": []string{strings.Repeat("c", 64)}}
	request["server"] = server
	return server
}

func generatedEnrollmentToken() []byte {
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(32))
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	base64.RawURLEncoding.Encode(encoded, secret)
	clear(secret)
	return encoded
}

func TestInstallEnrollmentConfiguration(t *testing.T) {
	installer, request, calls := nodeInstallFixture(t)
	enrollmentInstallRequest(request)
	writeInstallFixture(t, installer.fs, "/source/node-install.json", encodeFixture(t, request), 0600)
	if _, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode"); err == nil || len(*calls) != 0 {
		t.Fatal("未读取接入码仍安装")
	}
	token := generatedEnrollmentToken()
	defer clear(token)
	prompts := 0
	installer.readEnrollmentToken = func(context.Context) ([]byte, error) { prompts++; return bytes.Clone(token), nil }
	report := installFixture(t, installer)
	encoded, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/node.json"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := nodeapp.ParseConfig(encoded)
	if err != nil || node.Enrollment == nil || node.Enrollment.Registration.ImageID != report.ImageID || node.Enrollment.Registration.Name != "自动接入节点" || node.Enrollment.Registration.Capacity != 4 {
		t.Fatal("安装丢失接入信息", err)
	}
	tokenPath := filepath.Join(deploy.NodeDirectory, nodeEnrollmentFile)
	if node.Enrollment.Server.TokenFile != tokenPath {
		t.Fatal("未引用自动保存的接入码")
	}
	stored, err := os.ReadFile(installer.fs.resolve(tokenPath))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(stored)
	if !bytes.Equal(token, stored) {
		t.Fatal("保存的接入码与输入不一致")
	}
	stat, err := os.Stat(installer.fs.resolve(tokenPath))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("接入码权限不是私有", err)
	}
	for _, name := range []string{"installation.json", "node.json", "docker-install.json", "computer-image.json"} {
		content, err := os.ReadFile(installer.fs.resolve(filepath.Join(deploy.NodeDirectory, name)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, token) {
			t.Fatal("配置或安装记录泄露接入码", name)
		}
	}
	if second := installFixture(t, installer); second.NodeID != report.NodeID || second.ImageID != report.ImageID || prompts != 1 {
		t.Fatal("重试替换身份或重复询问凭据")
	}
}

func TestEnrollmentInputFailureDoesNotPublish(t *testing.T) {
	for _, kind := range []string{"取消", "空值", "非法编码", "错误长度", "非规范编码"} {
		t.Run(kind, func(t *testing.T) {
			installer, request, calls := nodeInstallFixture(t)
			enrollmentInstallRequest(request)
			writeInstallFixture(t, installer.fs, "/source/node-install.json", encodeFixture(t, request), 0600)
			installer.readEnrollmentToken = func(context.Context) ([]byte, error) {
				switch kind {
				case "取消":
					return nil, context.Canceled
				case "空值":
					return nil, nil
				case "非法编码":
					return bytes.Repeat([]byte("!"), 43), nil
				case "错误长度":
					return bytes.Repeat([]byte("a"), 44), nil
				default:
					return []byte(strings.Repeat("a", 42) + "B"), nil
				}
			}
			_, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode")
			if err == nil || len(*calls) != 0 {
				t.Fatal("无效输入仍启动服务")
			}
			if kind == "取消" && !errors.Is(err, context.Canceled) {
				t.Fatal("丢失取消原因")
			}
			if _, err := os.Stat(installer.fs.resolve(deploy.NodeDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("输入未完成仍发布配置")
			}
		})
	}
}

func TestEnrollmentRetryRejectsTampering(t *testing.T) {
	for _, kind := range []string{"权限", "内容", "删除"} {
		t.Run(kind, func(t *testing.T) {
			installer, request, calls := nodeInstallFixture(t)
			enrollmentInstallRequest(request)
			writeInstallFixture(t, installer.fs, "/source/node-install.json", encodeFixture(t, request), 0600)
			prompts := 0
			installer.readEnrollmentToken = func(context.Context) ([]byte, error) { prompts++; return generatedEnrollmentToken(), nil }
			// 服务启动失败后仍保留身份和凭据，使用原参数继续安装。
			systemctl := installer.systemctl
			installer.systemctl = func(context.Context, ...string) error { return errors.New("服务启动失败") }
			if _, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode"); err == nil {
				t.Fatal("服务失败被忽略")
			}
			installer.systemctl = systemctl
			installFixture(t, installer)
			path := installer.fs.resolve(filepath.Join(deploy.NodeDirectory, nodeEnrollmentFile))
			var err error
			switch kind {
			case "权限":
				err = os.Chmod(path, 0644)
			case "内容":
				err = os.WriteFile(path, generatedEnrollmentToken(), 0600)
			case "删除":
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			count := len(*calls)
			if _, err := installer.install(t.Context(), "/source/node-install.json", "/source/tomnode"); err == nil || prompts != 1 || len(*calls) != count {
				t.Fatal("修改后的凭据仍被接受或重复读取新凭据")
			}
		})
	}
}

func TestEnrollmentConfigurationBoundaries(t *testing.T) {
	_, request, _ := nodeInstallFixture(t)
	server := enrollmentInstallRequest(request)
	for _, field := range []string{"url", "node_url", "certificate_fingerprints", "enrollment_token_file"} {
		original, exists := server[field]
		server[field] = "非法值"
		parsed, _, err := parseNodeInstallRequest(encodeFixture(t, request))
		if err == nil {
			_, _, err = renderNodeInstallation(parsed, "linux/arm64")
		}
		if err == nil {
			t.Fatal("非法或过时接入配置被接受", field)
		}
		if exists {
			server[field] = original
		} else {
			delete(server, field)
		}
	}
	request["node"] = map[string]any{"enrollment": map[string]any{"server": map[string]any{"url": "https://other.example.test"}}}
	parsed, _, err := parseNodeInstallRequest(encodeFixture(t, request))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := renderNodeInstallation(parsed, "linux/arm64"); err == nil {
		t.Fatal("嵌套覆盖改写了顶层授权服务端")
	}
}
