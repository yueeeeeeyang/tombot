package serverapp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func catalogRequest() CatalogRequest {
	return CatalogRequest{Action: "register", Name: "正式节点", Capacity: 10, ImageID: ids.New(), ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64)}
}

func TestLoadCatalogRequest(t *testing.T) {
	request := catalogRequest()
	encoded, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "registration.json")
	writeTestFile(t, filename, encoded)
	loaded, err := LoadCatalogRequest(filename)
	if err != nil || loaded != request {
		t.Fatal("合法部署文件被拒绝", err)
	}
	status := `{"action":"status","image_id":"` + request.ImageID + `"}`
	writeTestFile(t, filename, []byte(" \n"+status+"\n"))
	if loaded, err := LoadCatalogRequest(filename); err != nil || loaded.Action != "status" {
		t.Fatal("合法查询文件被拒绝", err)
	}
	for _, invalid := range []string{
		`null`, `[]`, `{}`, status + `{}`, strings.Replace(status, "status", "ready", 1),
		strings.Replace(status, `"action"`, `"Action"`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"register","action":"status"`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"status","capacity":0`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"status","name":""`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"status","node_id":"`+ids.New()+`"`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"status","endpoint":"unix:///other"`, 1),
		strings.Replace(status, `"action":"status"`, `"action":"status","is_enabled":true`, 1),
		`{"action":"status","image_id":null}`,
		strings.Replace(string(encoded), `"capacity": 10`, `"capacity": "10"`, 1),
		strings.Replace(string(encoded), `"capacity": 10`, `"capacity": null`, 1),
		strings.Replace(string(encoded), `"register"`, `"status"`, 1),
		string([]byte{255}), strings.Repeat(" ", 16<<10) + status,
	} {
		writeTestFile(t, filename, []byte(invalid))
		if _, err := LoadCatalogRequest(filename); err == nil {
			t.Fatal("危险或含糊部署文件被接受", invalid)
		}
	}
	writeTestFile(t, filename, encoded)
	if err := os.Chmod(filename, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalogRequest(filename); err == nil {
		t.Fatal("组可写部署操作被接受")
	}
	link := filepath.Join(t.TempDir(), "registration-link.json")
	if err := os.Symlink(filename, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalogRequest(link); err == nil {
		t.Fatal("链接部署文件被接受")
	}
}

func TestCatalogDeploymentIntegration(t *testing.T) {
	cluster := pgtest.Start(t)
	configuration, _, pool, _ := prepareApplication(t, cluster)
	request := catalogRequest()
	result, err := RunCatalog(t.Context(), configuration, request)
	if err != nil || result.RequestID == "" || result.Action != "register" || result.Catalog.Node.ID != configuration.Node.ID || result.Catalog.Node.Status != "bootstrapping" || result.Catalog.Image.Reference != request.ImageReference {
		t.Fatal("部署登记未返回真实目录", err)
	}
	status := CatalogRequest{Action: "status", ImageID: request.ImageID}
	if result, err := RunCatalog(t.Context(), configuration, status); err != nil || result.Catalog.Node.Status != "bootstrapping" || result.Catalog.Image.ID != request.ImageID {
		t.Fatal("未查询同一登记", err)
	}
	create := bootstrapRequest(t)
	create.Create.NodeID = configuration.Node.ID
	create.Create.ImageID = request.ImageID
	if result, err := RunBootstrap(t.Context(), configuration, create); !errors.Is(err, accounts.ErrNoCapacity) || result.Operation != nil {
		t.Fatal("登记节点被误当成已就绪节点开户", err)
	}
	var users, operations int
	if err := pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.users),(SELECT count(*) FROM tombot.user_creation_operations)`).Scan(&users, &operations); err != nil || users != 0 || operations != 0 {
		t.Fatal("未就绪节点留下账户或开户操作", err)
	}
	request.Capacity++
	if _, err := RunCatalog(t.Context(), configuration, request); !errors.Is(err, nodecatalog.ErrConflict) {
		t.Fatal("重复部署静默修改节点容量", err)
	}
	if _, err := RunCatalog(t.Context(), configuration, CatalogRequest{Action: "status", ImageID: ids.New()}); !errors.Is(err, nodecatalog.ErrNotFound) {
		t.Fatal("未知镜像查询被当作成功", err)
	}
	var checksum string
	if err := pool.QueryRow(`SELECT checksum FROM tombot.schema_migrations WHERE version=44`).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE tombot.schema_migrations SET checksum=repeat('0',64) WHERE version=44`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(`UPDATE tombot.schema_migrations SET checksum=$1 WHERE version=44`, checksum); err != nil {
			t.Error(err)
		}
	})
	if result, err := RunCatalog(t.Context(), configuration, status); err == nil || result.Catalog.Node.ID != "" {
		t.Fatal("部署登记绕过数据库迁移账本校验", err)
	}
}
