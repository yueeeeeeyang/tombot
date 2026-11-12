package serverapp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func bootstrapRequest(t *testing.T) BootstrapRequest {
	t.Helper()
	return BootstrapRequest{Action: "create", Create: &accounts.CreateRequest{Username: "initial-admin", DisplayName: "首位管理员", NodeID: ids.New(), ImageID: ids.New(), IdempotencyKey: ids.New(), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, ProcessLimit: 128}}}
}

func TestBootstrapRequest(t *testing.T) {
	valid := bootstrapRequest(t)
	path := filepath.Join(t.TempDir(), "request.json")
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, encoded)
	if loaded, err := LoadBootstrapRequest(path); err != nil || loaded.Create.IdempotencyKey != valid.Create.IdempotencyKey {
		t.Fatal("未读取固定开户请求", err)
	}
	for _, content := range []string{"null", "{}", string(encoded) + " {}", `{"action":"status","operation_id":"invalid"}`, `{"action":"create","create":null}`, strings.Replace(string(encoded), `"role":""`, `"role":"admin"`, 1), strings.Replace(string(encoded), `"action":"create"`, `"action":"create","password":"forbidden-field"`, 1)} {
		writeTestFile(t, path, []byte(content))
		if _, err := LoadBootstrapRequest(path); err == nil {
			t.Fatal("接受错误部署请求")
		}
	}
	for _, action := range []string{"status", "retry", "reconcile", "activation"} {
		request := BootstrapRequest{Action: action, OperationID: ids.New()}
		if action == "activation" {
			request.ActivationFile = "/run/tombot-bootstrap/activation.json"
		}
		if request.validate() != nil {
			t.Fatal("拒绝有效操作", action)
		}
		request.Create = valid.Create
		if request.validate() == nil {
			t.Fatal("接受混合操作")
		}
	}
	if (localBootstrapAuthority{}).RequireAdmin(t.Context(), identity.Principal{Role: "admin"}, false) != identity.ErrForbidden {
		t.Fatal("本机引导允许普通管理入口")
	}
}

func testActivation(t *testing.T) accounts.Activation {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal(err)
	}
	return accounts.Activation{UserID: ids.New(), Value: identity.Credential(value), ExpiresAt: time.Now().Add(time.Hour)}
}

func privateBootstrapDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestActivationFile(t *testing.T) {
	t.Run("持久私有文件与不覆盖", func(t *testing.T) {
		path := filepath.Join(privateBootstrapDirectory(t), "activation.json")
		output, err := reserveActivationFile(path)
		if err != nil {
			t.Fatal(err)
		}
		activation := testActivation(t)
		if err = output.save(activation); err != nil {
			t.Fatal(err)
		}
		if err = output.close(); err != nil {
			t.Fatal(err)
		}
		encoded, err := readFile(path, 4096, true)
		if err != nil {
			t.Fatal(err)
		}
		var saved accounts.Activation
		if json.Unmarshal(encoded, &saved) != nil || saved.Value != activation.Value || saved.UserID != activation.UserID {
			t.Fatal("激活材料未完整交付")
		}
		if _, err := reserveActivationFile(path); err == nil {
			t.Fatal("允许覆盖已交付文件")
		}
	})
	t.Run("失败清理空文件", func(t *testing.T) {
		path := filepath.Join(privateBootstrapDirectory(t), "activation.json")
		output, err := reserveActivationFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = output.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("失败遗留空文件")
		}
	})
	t.Run("拒绝链接和公开目录", func(t *testing.T) {
		directory := privateBootstrapDirectory(t)
		path := filepath.Join(directory, "activation.json")
		if err := os.Symlink(filepath.Join(directory, "missing"), path); err != nil {
			t.Fatal(err)
		}
		if _, err := reserveActivationFile(path); err == nil {
			t.Fatal("跟随了输出链接")
		}
		if err := os.Chmod(directory, 0750); err != nil {
			t.Fatal(err)
		}
		if _, err := reserveActivationFile(filepath.Join(directory, "new")); err == nil {
			t.Fatal("接受非私有目录")
		}
	})
	t.Run("替换路径不交付且不误删", func(t *testing.T) {
		directory := privateBootstrapDirectory(t)
		path := filepath.Join(directory, "activation.json")
		output, err := reserveActivationFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(directory, "moved")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, path, []byte("保留其他文件"))
		if output.save(testActivation(t)) == nil || output.close() == nil {
			t.Fatal("未报告路径身份改变")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "保留其他文件" {
			t.Fatal("误删替换内容", err)
		}
	})
	t.Run("硬链接阻止写入", func(t *testing.T) {
		directory := privateBootstrapDirectory(t)
		path := filepath.Join(directory, "activation.json")
		output, err := reserveActivationFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, filepath.Join(directory, "copy")); err != nil {
			t.Fatal(err)
		}
		if output.save(testActivation(t)) == nil {
			t.Fatal("材料写入了多链接文件")
		}
		if err = output.close(); err != nil {
			t.Fatal(err)
		}
	})
}

// bootstrapComputer 明确只替代 Docker 副作用，账户、租约、审计与激活均使用真实 PostgreSQL。
type bootstrapComputer struct {
	state string
	fail  bool
}

func (node *bootstrapComputer) Inspect(_ context.Context, specification computer.Specification) (computer.Observation, error) {
	state := node.state
	if state == "" {
		state = "absent"
	}
	observation := computer.Observation{OperationID: specification.OperationID, UserID: specification.UserID, ComputerID: specification.ComputerID, Generation: specification.Generation, FencingToken: specification.FencingToken, VolumeReference: specification.VolumeReference, State: state}
	if state != "absent" {
		observation.ContainerID = secure.Hash(specification.ComputerID)
		observation.Capabilities = computer.Capabilities{Terminal: true, Browser: true, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}
	}
	return observation, nil
}

func (node *bootstrapComputer) Ensure(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	if node.fail {
		return computer.Observation{}, &computer.Failure{Code: "bootstrap_test_failure", IsPermanent: true}
	}
	node.state = "ready"
	return node.Inspect(ctx, specification)
}

func (node *bootstrapComputer) Remove(ctx context.Context, specification computer.Specification) (computer.Observation, error) {
	node.state = "absent"
	return node.Inspect(ctx, specification)
}

func TestBootstrapIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	configuration, roots, pool, _ := prepareApplication(t, cluster)
	configuration.Provision.RequestWait = Duration(20 * time.Millisecond)
	configuration.Provision.PollInterval = Duration(time.Millisecond)
	configuration.Provision.RetryDelay = Duration(time.Millisecond)
	request := bootstrapRequest(t)
	request.Create.NodeID = configuration.Node.ID
	if _, err := pool.Exec(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, request.Create.ImageID, "registry.example.test/computer@sha256:"+secure.Hash(ids.New())); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',2,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, configuration.Node.ID, request.Create.ImageID); err != nil {
		t.Fatal(err)
	}
	result, err := RunBootstrap(t.Context(), configuration, request)
	if !errors.Is(err, accounts.ErrPending) || result.Operation == nil || result.Operation.Status != "pending" {
		t.Fatal("工作池未启动时未返回持久操作", err)
	}
	operation := *result.Operation
	status := BootstrapRequest{Action: "status", OperationID: operation.ID}
	if result, err = RunBootstrap(t.Context(), configuration, status); err != nil || result.Operation.ID != operation.ID || result.Operation.Status != "pending" {
		t.Fatal("未查询同一操作", err)
	}
	activation := BootstrapRequest{Action: "activation", OperationID: operation.ID, ActivationFile: filepath.Join(privateBootstrapDirectory(t), "activation.json")}
	if _, err = RunBootstrap(t.Context(), configuration, activation); !errors.Is(err, accounts.ErrActivationUnavailable) {
		t.Fatal("电脑未就绪即可签发", err)
	}
	if _, err = os.Lstat(activation.ActivationFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("签发失败遗留输出")
	}
	connections, err := configuration.Database.connections([]postgres.Role{postgres.ControlRole})
	if err != nil {
		t.Fatal(err)
	}
	database, err := configuration.Database.open(t.Context(), postgres.ControlRole, connections[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	worker, err := accounts.New(database, &bootstrapComputer{}, localBootstrapAuthority{}, configuration.Provision.options())
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := worker.ProcessNext(t.Context()); err != nil || !claimed {
		t.Fatal("未执行同步开户", err)
	}
	if result, err = RunBootstrap(t.Context(), configuration, request); err != nil || result.Operation.ID != operation.ID || result.Operation.Status != "succeeded" {
		t.Fatal("重试同一请求没有返回原成功操作", err)
	}
	if result, err = RunBootstrap(t.Context(), configuration, activation); err != nil || !result.ActivationSaved || result.ActivationUserID != operation.UserID {
		t.Fatal("未交付首位管理员材料", err)
	}
	encoded, err := os.ReadFile(activation.ActivationFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved accounts.Activation
	if json.Unmarshal(encoded, &saved) != nil || len(saved.Value) == 0 {
		t.Fatal("激活文件内容无效")
	}
	receipt, err := json.Marshal(result)
	if err != nil || strings.Contains(string(receipt), string(saved.Value)) || strings.Contains(string(receipt), "activation_token") {
		t.Fatal("普通回执泄露材料")
	}
	if _, err := RunBootstrap(t.Context(), configuration, activation); err == nil {
		t.Fatal("已有输出文件仍然重新签发")
	}
	digest := sha256.Sum256([]byte(saved.Value))
	var active int
	if err := pool.QueryRow(`SELECT count(*) FROM tombot.activation_tokens WHERE user_id=$1 AND token_digest=$2 AND consumed_at IS NULL`, operation.UserID, digest[:]).Scan(&active); err != nil || active != 1 {
		t.Fatal("文件冲突使原材料失效", err)
	}
	request.Create.IdempotencyKey = ids.New()
	if _, err := RunBootstrap(t.Context(), configuration, request); !errors.Is(err, accounts.ErrAlreadyInitialized) {
		t.Fatal("允许初始化第二名管理员", err)
	}
	request.Create.NodeID = ids.New()
	if _, err := RunBootstrap(t.Context(), configuration, request); err == nil {
		t.Fatal("允许其他节点")
	}
	status.OperationID = ids.New()
	if _, err := RunBootstrap(t.Context(), configuration, status); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatal("未知操作被解释为成功", err)
	}
	verifyBootstrapActivation(t, configuration, roots, saved, request.Create.Username)
}
