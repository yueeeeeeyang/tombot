package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/deploy"
	"io.yueyang/tom/internal/dockerbootstrap"
)

func legacyRepairFixture(t *testing.T) (*nodeInstaller, []byte, map[string][]byte) {
	t.Helper()
	installer, _, _ := automaticFixture(t)
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err != nil {
		t.Fatal(err)
	}
	path := deploy.NodeDirectory + "/installation.json"
	content, err := os.ReadFile(installer.fs.resolve(path))
	if err != nil {
		t.Fatal(err)
	}
	var record nodeInstallation
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatal(err)
	}
	request, _, err := parseNodeInstallRequest(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"engine_package_version":"5:29.9.3-1~ubuntu.24.04~noble","e2fsprogs_package_version":"1.47.0-2.4~exp1ubuntu4.1"}`)
	request.Docker = legacy
	record.Request = encodeFixture(t, request)
	_, record.RequestSHA256, err = parseNodeInstallRequest(record.Request)
	if err != nil {
		t.Fatal(err)
	}
	record.Files["docker-install.json"] = contentDigest(legacy)
	writeInstallFixture(t, installer.fs, deploy.NodeDirectory+"/docker-install.json", legacy, 0600)
	// 使用原安装摘要模拟尚未加入 Docker 停服顺序的受管单元。
	legacyService := bytes.Replace([]byte(deploy.NodeService()), []byte("After=docker.service\n"), nil, 1)
	record.ServiceSHA256 = contentDigest(legacyService)
	writeInstallFixture(t, installer.fs, nodeServicePath, legacyService, 0644)
	writeInstallFixture(t, installer.fs, path, encodeFixture(t, record), 0600)
	protected := map[string][]byte{}
	for _, name := range []string{"certificate.pem", "private.key", "enrollment.token", "computer-image.tar"} {
		content, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/" + name))
		if err != nil {
			t.Fatal(err)
		}
		protected[name] = content
	}
	program, err := os.ReadFile(installer.fs.resolve("/source/tomnode"))
	if err != nil {
		t.Fatal(err)
	}
	program = append(program, 1)
	writeInstallFixture(t, installer.fs, "/source/tomnode", program, 0755)
	return installer, program, protected
}

func stoppedNode(context.Context) error { return nil }

func TestNodeRepairMigratesLegacyManifestAndPreservesIdentity(t *testing.T) {
	installer, _, protected := legacyRepairFixture(t)
	options := NodeRepairOptions{Listen: "10.0.0.5:9444", URL: "https://node.example.test:9444"}
	report, err := installer.repair(t.Context(), "/source/tomnode", options, stoppedNode)
	if err != nil || report.State != "control_started" || report.Listen != options.Listen {
		t.Fatal(report, err)
	}
	for name, expected := range protected {
		content, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/" + name))
		if err != nil || !bytes.Equal(content, expected) {
			t.Fatal("修复改变了身份或归档", name, err)
		}
	}
	manifest, err := os.ReadFile(installer.fs.resolve(deploy.NodeDirectory + "/docker-install.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dockerbootstrap.ParseManifest(manifest); err != nil {
		t.Fatal(err)
	}
	service, err := os.ReadFile(installer.fs.resolve(nodeServicePath))
	if err != nil || string(service) != deploy.NodeService() || !bytes.Contains(service, []byte("After=docker.service\n")) {
		t.Fatal("修复未安装带 Docker 停服顺序的新节点单元", err)
	}
	backups, err := filepath.Glob(installer.fs.resolve(nodeRepairDirectory + "-*/plan.json"))
	if err != nil || len(backups) != 1 {
		t.Fatal("缺少完整修复备份", err)
	}
	if _, err := installer.install(t.Context(), "", "/source/tomnode"); err != nil {
		t.Fatal("修复后不能用原安装方式重试", err)
	}
}

func TestNodeRepairRecoversEveryReplacementBoundary(t *testing.T) {
	for count := 0; count <= len(nodeRepairTargets()); count++ {
		installer, program, _ := legacyRepairFixture(t)
		original, request := writeSharedBrowserFixture(t, installer)
		options := NodeRepairOptions{Listen: "10.0.0.5:9444", URL: "https://node.example.test:9444"}
		if err := installer.prepareNodeRepair(t.Context(), program, options); err != nil {
			t.Fatal(err)
		}
		for _, target := range nodeRepairTargets()[:count] {
			content, err := os.ReadFile(installer.fs.resolve(nodeRepairDirectory + "/after/" + target.name))
			if err != nil {
				t.Fatal(err)
			}
			if err := (serverInstaller{fs: installer.fs}).replaceServerFile(target.path, content, target.mode); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := installer.repair(t.Context(), "/source/tomnode", options, stoppedNode); err != nil {
			t.Fatal("未恢复中断点", count, err)
		}
		checkSharedBrowserMigration(t, installer, original, request.NodeID)
	}
}

func TestNodeRepairRefusesDriftAndRunningServices(t *testing.T) {
	for _, scenario := range []string{"运行中", "端口占用", "备份变化", "目标变化", "身份变化", "参数变化", "取消"} {
		t.Run(scenario, func(t *testing.T) {
			installer, program, _ := legacyRepairFixture(t)
			options := NodeRepairOptions{Listen: "10.0.0.5:9444", URL: "https://node.example.test:9444"}
			if err := installer.prepareNodeRepair(t.Context(), program, options); err != nil {
				t.Fatal(err)
			}
			check := stoppedNode
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "运行中":
				check = func(context.Context) error { return errors.New("仍在运行") }
			case "端口占用":
				installer.checkListen = func(context.Context, string) error { return errors.New("端口占用") }
			case "备份变化":
				writeInstallFixture(t, installer.fs, nodeRepairDirectory+"/after/program", []byte("漂移"), 0600)
			case "目标变化":
				writeInstallFixture(t, installer.fs, deploy.NodeDirectory+"/node.json", []byte("漂移"), 0600)
			case "身份变化":
				writeInstallFixture(t, installer.fs, deploy.NodeDirectory+"/private.key", []byte("漂移"), 0600)
			case "参数变化":
				options.Listen = "10.0.0.5:9445"
			case "取消":
				cancel()
			}
			before, err := os.ReadFile(installer.fs.resolve(nodeBinaryPath))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := installer.repair(ctx, "/source/tomnode", options, check); err == nil {
				t.Fatal("错误状态仍修复成功")
			}
			after, err := os.ReadFile(installer.fs.resolve(nodeBinaryPath))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("失败前覆盖了原程序", err)
			}
		})
	}
}
