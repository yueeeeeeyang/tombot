//go:build linux && deploymentintegration

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLiveNodeConfigurationCommand 在无 API 文件的隔离容器中调用正式二进制，不访问宿主 Docker 或安装软件。
func TestLiveNodeConfigurationCommand(t *testing.T) {
	executable := os.Getenv("TOM_NODE_BINARY")
	if os.Geteuid() != 0 || executable == "" || os.Getenv("TMPDIR") != "/run" {
		t.Fatal("需要隔离 root 容器、正式节点程序及受保护的 /run 临时目录")
	}
	fs, value, paths := nodeFilesystemFixture(t)
	value.node.Control.CertificateFile = fs.resolve(value.node.Control.CertificateFile)
	value.node.Control.PrivateKeyFile = fs.resolve(value.node.Control.PrivateKeyFile)
	value.image.Platform = "linux/" + runtime.GOARCH
	if err := os.WriteFile(fs.resolve(paths.Node), encodeFixture(t, value.node), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fs.resolve(paths.Image), encodeFixture(t, value.image), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	environment := []string{"TOM_NODE_CONFIG=" + fs.resolve(paths.Node), "TOM_DOCKER_INSTALL_MANIFEST=" + fs.resolve(paths.Docker), "TOM_IMAGE_PREPARATION_MANIFEST=" + fs.resolve(paths.Image)}
	command := exec.CommandContext(ctx, executable, "check")
	command.Env = environment
	var output, diagnostics bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		t.Fatal("正式节点检查失败", err, diagnostics.String())
	}
	var report Report
	if json.Unmarshal(output.Bytes(), &report) != nil || report.Scope != "node_configuration" || report.NodeID != value.node.NodeID {
		t.Fatal("节点检查回执无效")
	}
	if err := os.Chmod(value.node.Control.PrivateKeyFile, 0644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	diagnostics.Reset()
	command = exec.CommandContext(ctx, executable, "start")
	command.Env, command.Stdout, command.Stderr = environment, &output, &diagnostics
	if err := command.Run(); err == nil || ctx.Err() != nil || output.Len() != 0 || !strings.Contains(diagnostics.String(), "节点部署配置检查未通过") || strings.Contains(diagnostics.String(), "准备 Docker") {
		t.Fatal("正式启动未在配置错误时阻止宿主安装", err, diagnostics.String())
	}
}
