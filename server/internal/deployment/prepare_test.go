package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/serverapp"
)

func preparationFixture(t *testing.T) (map[string]any, account) {
	t.Helper()
	value, _, api := fixtureConfiguration(t)
	// 此清单只用于解析测试，不作为宿主安装版本推荐或下载来源。
	docker := dockerbootstrap.Manifest{EnginePackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", CLIPackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", ContainerdPackageVersion: "2.3.6-1~ubuntu.24.04~noble", E2fsprogsPackageVersion: "1.47.0-test1", UtilLinuxPackageVersion: "2.39.3-test1", MountPackageVersion: "2.39.3-test1", AllowedEngineVersions: []string{"29.7.2"}, RepositoryFingerprint: strings.Repeat("A", 40), TimeoutSeconds: 300, MinimumFreeBytes: 1 << 30}
	return map[string]any{"version": 1, "node_id": value.node.NodeID, "node_name": "测试电脑节点", "computer_capacity": 4, "image_id": value.image.ImageID, "image": map[string]any{"reference": value.image.Reference}, "docker": docker, "server": map[string]any{"address": "127.0.0.1:8443", "admin_origin": "https://127.0.0.1:8443", "models": map[string]any{"origins": []map[string]any{{"url": "https://model.example.test"}}}}}, api
}

func generatedFile(t *testing.T, files []configurationFile, name string) []byte {
	t.Helper()
	for _, file := range files {
		if file.name == name {
			return file.content
		}
	}
	t.Fatal("生成文件缺失", name)
	return nil
}

func TestRenderConfigurationBindsAllIdentitiesAndDefaults(t *testing.T) {
	request, api := preparationFixture(t)
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		files, report, err := renderConfiguration(encodeFixture(t, request), api, platform)
		if err != nil || len(files) != 5 || report.Scope != "deployment_configuration_files" {
			t.Fatal("生成失败", report, err)
		}
		server, err := serverapp.ParseConfig(generatedFile(t, files, "server.json"))
		if err != nil {
			t.Fatal(err)
		}
		node, err := nodeapp.ParseConfig(generatedFile(t, files, "node.json"))
		if err != nil {
			t.Fatal(err)
		}
		image, err := dockernode.ParseImageManifest(generatedFile(t, files, "computer-image.json"))
		if err != nil {
			t.Fatal(err)
		}
		var registration serverapp.CatalogRequest
		if err := json.Unmarshal(generatedFile(t, files, "node-registration.json"), &registration); err != nil {
			t.Fatal(err)
		}
		if node.NodeID != report.NodeID || server.Node.ID != node.NodeID || image.NodeID != node.NodeID || image.ImageID != registration.ImageID || server.Node.Verification.ImageID != image.ImageID || image.Platform != platform || node.ClientUID != api.uid || node.ClientGID != api.gid || node.Runtime.MaximumLease != 300000 || registration.Action != "register" {
			t.Fatal("生成配置未保持统一身份和基线")
		}
	}
}

func TestRenderConfigurationRejectsConflicts(t *testing.T) {
	for _, kind := range []string{"version", "node-id", "uid", "server-node", "image", "lease", "path", "docker", "capacity", "empty-keys", "empty-dsn", "registration-path", "alias", "model-origin", "missing-origins"} {
		t.Run(kind, func(t *testing.T) {
			request, api := preparationFixture(t)
			server := request["server"].(map[string]any)
			switch kind {
			case "version":
				request["version"] = 2
			case "node-id":
				request["node_id"] = "invalid"
			case "uid":
				request["node"] = map[string]any{"client_uid": api.uid + 1}
			case "server-node":
				server["node"] = map[string]any{"id": "invalid"}
			case "image":
				request["image"] = map[string]any{"reference": "example.test/computer:latest"}
			case "lease":
				request["node"] = map[string]any{"runtime": map[string]any{"maximum_lease_ms": 120000}}
			case "path":
				server["admin_directory"] = "/etc/tombot"
			case "docker":
				request["docker"] = map[string]any{}
			case "capacity":
				request["computer_capacity"] = 8
			case "empty-keys":
				server["model_key_files"] = map[string]any{}
			case "empty-dsn":
				server["database"] = map[string]any{"dsn_files": map[string]any{}}
			case "registration-path":
				server["identity_key_file"] = "/etc/tombot/node-registration.json"
			case "alias":
				server["Admin_Origin"] = "https://unexpected.example.test"
			case "model-origin":
				server["models"] = map[string]any{"origins": []map[string]string{{"url": "https://operator@model.example.test"}}}
			case "missing-origins":
				delete(server, "models")
			}
			if files, report, err := renderConfiguration(encodeFixture(t, request), api, "linux/arm64"); err == nil || files != nil || report.Scope != "" {
				t.Fatal("冲突配置产生交付结果", kind, err)
			}
		})
	}
}

func TestPreparationJSONRejectsAmbiguity(t *testing.T) {
	request, api := preparationFixture(t)
	valid := encodeFixture(t, request)
	for _, body := range [][]byte{
		append([]byte(`{"version":1,`), valid[1:]...),
		append(bytes.Clone(valid), []byte(` {}`)...),
		bytes.Replace(valid, []byte(`"version":1`), []byte(`"Version":1`), 1),
		bytes.Replace(valid, []byte(`"computer_capacity":4`), []byte(`"computer_capacity":null`), 1),
		bytes.Replace(valid, []byte(`"address":"127.0.0.1:8443"`), []byte(`"address":"127.0.0.1:8443","address":"127.0.0.1:8443"`), 1),
		bytes.Replace(valid, []byte(`"engine_package_version"`), []byte(`"ENGINE_PACKAGE_VERSION"`), 1),
		[]byte(`{"node":[[[[[[[[[[[[[[[[[[0]]]]]]]]]]]]]]]]]]}`),
		bytes.Repeat([]byte(" "), (256<<10)+1),
		[]byte{0xff},
	} {
		if files, report, err := renderConfiguration(body, api, "linux/arm64"); err == nil || files != nil || report.Scope != "" {
			t.Fatal("歧义参数被接受")
		}
	}
}

func TestExplicitKeyMapReplacesDefault(t *testing.T) {
	request, api := preparationFixture(t)
	server := request["server"].(map[string]any)
	server["active_model_key"], server["model_key_files"] = "rotated", map[string]string{"rotated": "/etc/tombot/private/rotated.key"}
	files, _, err := renderConfiguration(encodeFixture(t, request), api, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := serverapp.ParseConfig(generatedFile(t, files, "server.json"))
	if err != nil || len(configuration.ModelKeyFiles) != 1 || configuration.ModelKeyFiles["current"] != "" {
		t.Fatal("显式映射混入默认引用", err)
	}
}

func TestConfigurationPublicationIsCompleteAndNeverOverwrites(t *testing.T) {
	request, api := preparationFixture(t)
	files, _, err := renderConfiguration(encodeFixture(t, request), api, "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	fs := filesystem{base: t.TempDir(), owner: uint32(os.Geteuid())}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Go(func() { results <- publishConfiguration(t.Context(), fs, "/output", files) })
	}
	wait.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatal("并发发布未保持单一成功", succeeded)
	}
	for _, file := range files {
		path := fs.resolve("/output/" + file.name)
		body, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(body, file.content) {
			t.Fatal("发布内容缺失或不完整", err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != file.mode {
			t.Fatal("发布权限不符", err)
		}
	}
	entries, err := os.ReadDir(fs.base)
	if err != nil || len(entries) != 1 || entries[0].Name() != "output" {
		t.Fatal("发布遗留暂存文件", err)
	}
	if err := os.Mkdir(fs.resolve("/empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if publishConfiguration(t.Context(), fs, "/empty", files) == nil {
		t.Fatal("覆盖了既有空目录")
	}
}

func TestConfigurationPublicationFailureLeavesNoPartialResult(t *testing.T) {
	for _, kind := range []string{"cancel", "write", "symlink", "parent"} {
		t.Run(kind, func(t *testing.T) {
			request, api := preparationFixture(t)
			files, _, err := renderConfiguration(encodeFixture(t, request), api, "linux/arm64")
			if err != nil {
				t.Fatal(err)
			}
			fs := filesystem{base: t.TempDir(), owner: uint32(os.Geteuid())}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			destination := "/output"
			switch kind {
			case "cancel":
				cancel()
			case "write":
				files[4].name = files[0].name
			case "symlink":
				if err := os.Symlink("/outside", fs.resolve(destination)); err != nil {
					t.Fatal(err)
				}
			case "parent":
				if err := os.Mkdir(fs.resolve("/unsafe"), 0777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(fs.resolve("/unsafe"), 0777); err != nil {
					t.Fatal(err)
				}
				destination = "/unsafe/output"
			}
			if publishConfiguration(ctx, fs, destination, files) == nil {
				t.Fatal("错误发布产生成功结果")
			}
			if kind != "symlink" {
				if _, err := os.Lstat(fs.resolve(destination)); !os.IsNotExist(err) {
					t.Fatal("失败留下可消费的输出", err)
				}
			}
			stages, err := filepath.Glob(filepath.Join(fs.base, ".tombot-configuration-*"))
			if err != nil || len(stages) != 0 {
				t.Fatal("失败暂存目录未回收", err)
			}
		})
	}
}
