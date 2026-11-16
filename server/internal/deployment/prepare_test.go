package deployment

import (
	"bytes"
	"context"

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
	docker := dockerbootstrap.DefaultManifest()
	return map[string]any{"version": 1, "node_id": value.node.NodeID, "node_name": "测试电脑节点", "computer_capacity": 4, "image_id": value.image.ImageID, "image": map[string]any{"reference": value.image.Reference}, "docker": docker, "control": map[string]any{"listen": "10.0.0.5:9443", "allow_public_listen": false, "api_certificate_fingerprint": strings.Repeat("b", 64)}, "server": map[string]any{"address": "127.0.0.1:8443", "admin_origin": "https://127.0.0.1:8443"}}, api
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
		if server.Database.MigrationDSNFile != "/etc/tombot/private/migration.connection" {
			t.Fatal("生成配置缺少独立迁移连接引用")
		}
		node, err := nodeapp.ParseConfig(generatedFile(t, files, "node.json"))
		if err != nil {
			t.Fatal(err)
		}
		image, err := dockernode.ParseImageManifest(generatedFile(t, files, "computer-image.json"))
		if err != nil {
			t.Fatal(err)
		}
		if node.NodeID != report.NodeID || image.NodeID != node.NodeID || image.Platform != platform || node.Runtime.MaximumLease != 300000 {
			t.Fatal("生成配置未保持统一身份和基线")
		}
		var options NodeInstallOptions
		if err := decodePreparation(generatedFile(t, files, "node-install.json"), &options); err != nil {
			t.Fatal(err)
		}
		if options.Listen != node.Listen {
			t.Fatal("高级参数丢失明确的监听地址")
		}
		if node.Enrollment != nil && (options.Name != node.Enrollment.Registration.Name || options.Capacity != node.Enrollment.Registration.Capacity || options.URL != node.Enrollment.Server.NodeURL) {
			t.Fatal("高级参数丢失节点差异配置")
		}
		if bytes.Contains(generatedFile(t, files, "node-install.json"), []byte("migration.connection")) {
			t.Fatal("节点安装参数混入主服务数据库引用")
		}
	}
}

func TestRenderConfigurationRejectsConflicts(t *testing.T) {
	for _, kind := range []string{"version", "node-id", "server-node", "image", "lease", "path", "obsolete-admin-directory", "docker", "capacity", "empty-keys", "empty-dsn", "empty-migration-dsn", "registration-path", "alias", "obsolete-origins", "model-budget"} {
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
				request["node"] = map[string]any{"runtime": map[string]any{"state_directory": "/etc/tombot"}}
			case "obsolete-admin-directory":
				server["admin_directory"] = "/opt/tombot/admin"
			case "docker":
				request["docker"] = map[string]any{}
			case "capacity":
				request["computer_capacity"] = 8
			case "empty-keys":
				server["model_key_files"] = map[string]any{}
			case "empty-dsn":
				server["database"] = map[string]any{"dsn_files": map[string]any{}}
			case "empty-migration-dsn":
				server["database"] = map[string]any{"migration_dsn_file": ""}
			case "registration-path":
				server["identity_key_file"] = "/etc/tombot/node-install.json"
			case "alias":
				server["Admin_Origin"] = "https://unexpected.example.test"
			case "obsolete-origins":
				server["models"] = map[string]any{"origins": []map[string]string{{"url": "https://operator@model.example.test"}}}
			case "model-budget":
				server["models"] = map[string]any{"concurrency": 0}
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
		bytes.Replace(valid, []byte(`"timeout_seconds"`), []byte(`"TIMEOUT_SECONDS"`), 1),
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
