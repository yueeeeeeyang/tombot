//go:build linux && deploymentintegration

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/serverapp"
)

// TestLiveDeploymentCheck 使用容器内实际系统账户及权限；不连接数据库、Docker 或宿主资源。
func TestLiveDeploymentCheck(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("真实部署配置验收必须使用专用 root 测试容器")
	}
	api, err := lookupAccount()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/run", "tombot-deployment-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	value, paths, _ := fixtureConfiguration(t)
	relocate := func(path string) string { return filepath.Join(base, strings.TrimPrefix(path, "/")) }
	paths = Paths{Server: relocate(paths.Server), Node: relocate(paths.Node), Docker: relocate(paths.Docker), Image: relocate(paths.Image)}
	value.server.AdminDirectory = relocate(value.server.AdminDirectory)
	value.server.TLSCertificateFile = relocate(value.server.TLSCertificateFile)
	value.server.TLSPrivateKeyFile = relocate(value.server.TLSPrivateKeyFile)
	value.server.IdentityKeyFile = relocate(value.server.IdentityKeyFile)
	value.server.CSRFKeyFile = relocate(value.server.CSRFKeyFile)
	for name, path := range value.server.ModelKeyFiles {
		value.server.ModelKeyFiles[name] = relocate(path)
	}
	for role, path := range value.server.Database.DSNFiles {
		value.server.Database.DSNFiles[role] = relocate(path)
	}
	value.image.Platform = "linux/" + runtime.GOARCH
	fs := filesystem{base: "/", owner: 0}
	materializeFixture(t, fs, value, paths, api)
	report, err := Check(t.Context(), paths)
	if err != nil || report.Scope != "deployment_configuration" || report.APIUID != api.uid {
		t.Fatal("实际身份配置未通过", report, err)
	}
	if err := os.Chmod(paths.Server, 0600); err != nil {
		t.Fatal(err)
	}
	if report, err := Check(t.Context(), paths); err == nil || report.Scope != "" {
		t.Fatal("API 无权读取配置却通过检查")
	}
	if err := os.Chmod(paths.Server, 0644); err != nil {
		t.Fatal(err)
	}
	value.node.Runtime.MaximumLease = 120000
	writeFixture(t, fs, paths.Node, encodeFixture(t, value.node), 0600, 0)
	if report, err := Check(t.Context(), paths); err == nil || report.Scope != "" || !strings.Contains(err.Error(), "租约") {
		t.Fatal("旧节点示例租约错误未被拦截", err)
	}
	value.node.Runtime.MaximumLease = 300000
	writeFixture(t, fs, paths.Node, encodeFixture(t, value.node), 0600, 0)
	if err := os.Chown(value.server.IdentityKeyFile, 0, 0); err != nil {
		t.Fatal(err)
	}
	if report, err := Check(t.Context(), paths); err == nil || report.Scope != "" {
		t.Fatal("API 无权读取私有引用却通过检查")
	}
}

// TestLiveConfigurationCommand 消费真实生成程序的输出，再调用正式部署检查；引用内容仅为元信息测试夹具。
func TestLiveConfigurationCommand(t *testing.T) {
	api, err := lookupAccount()
	if err != nil || os.Geteuid() != 0 {
		t.Fatal("真实配置生成验收需要专用 root 测试容器", err)
	}
	base, err := os.MkdirTemp("/run", "tombot-config-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	request, _ := preparationFixture(t)
	server := request["server"].(map[string]any)
	server["admin_directory"] = filepath.Join(base, "admin")
	server["tls_certificate_file"] = filepath.Join(base, "certificate.pem")
	server["tls_private_key_file"] = filepath.Join(base, "private/tls.key")
	server["identity_key_file"] = filepath.Join(base, "private/identity.key")
	server["csrf_key_file"] = filepath.Join(base, "private/csrf.key")
	server["model_key_files"] = map[string]string{"current": filepath.Join(base, "private/model.key")}
	connections := map[string]string{}
	for _, role := range []string{"identity", "control", "tenant", "dispatch"} {
		connections["tombot_"+role] = filepath.Join(base, "private/"+role+".connection")
	}
	server["database"] = map[string]any{"dsn_files": connections}
	source, destination := filepath.Join(base, "request.json"), filepath.Join(base, "configuration")
	if err := os.WriteFile(source, encodeFixture(t, request), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/verification/tomdeploy-config")
	command.Env = []string{"TOM_DEPLOYMENT_REQUEST=" + source, "TOM_DEPLOYMENT_OUTPUT=" + destination}
	var output, diagnostics bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		t.Fatal("正式配置生成命令失败", err, diagnostics.String())
	}
	var report Report
	if json.Unmarshal(output.Bytes(), &report) != nil || report.Scope != "deployment_configuration_files" || report.APIUID != api.uid {
		t.Fatal("正式命令回执无效")
	}
	paths := Paths{Server: filepath.Join(destination, "server.json"), Node: filepath.Join(destination, "node.json"), Docker: filepath.Join(destination, "docker-install.json"), Image: filepath.Join(destination, "computer-image.json")}
	if _, err := Check(t.Context(), paths); err == nil {
		t.Fatal("未准备凭据引用时不应通过安装前检查")
	}
	var value configuration
	if value.server, err = serverapp.LoadConfig(paths.Server); err != nil {
		t.Fatal(err)
	}
	if value.node, err = nodeapp.LoadConfig(paths.Node); err != nil {
		t.Fatal(err)
	}
	imageContent, err := os.ReadFile(paths.Image)
	if err != nil {
		t.Fatal(err)
	}
	if value.image, err = dockernode.ParseImageManifest(imageContent); err != nil {
		t.Fatal(err)
	}
	registration, err := serverapp.LoadCatalogRequest(filepath.Join(destination, "node-registration.json"))
	if err != nil || registration.ImageID != report.ImageID {
		t.Fatal("登记命令不能读取生成的配置", err)
	}
	for _, reference := range value.references() {
		writeFixture(t, filesystem{base: "/", owner: 0}, reference.path, make([]byte, 32), 0600, api.uid)
	}
	if err := os.MkdirAll(value.server.AdminDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(t.Context(), paths); err != nil {
		t.Fatal("生成结果无法通过正式配置检查", err)
	}
	before, err := os.ReadFile(paths.Server)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(t.Context(), source, destination); err == nil {
		t.Fatal("正式入口覆盖既有输出目录")
	}
	after, err := os.ReadFile(paths.Server)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("重复运行改变了原配置", err)
	}
}
