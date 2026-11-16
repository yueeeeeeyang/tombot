package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestAdminCatalog(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t), true)
	ctx := context.Background()
	operation := fixture.reserve(t)
	first, err := fixture.service.ListUsers(ctx, fixture.actor, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != operation.UserID || first[0].CreationOperationID == nil || *first[0].CreationOperationID != operation.ID || first[0].ComputerStatus == nil || *first[0].ComputerStatus != "pending" {
		t.Fatal("账户目录未关联创建进度", first, err)
	}
	second, err := fixture.service.ListUsers(ctx, fixture.actor, first[0].ID, 1)
	if err != nil || len(second) != 1 || second[0].ID != fixture.actor.UserID || second[0].CreationOperationID != nil {
		t.Fatal("账户目录分页重复或遗漏", second, err)
	}
	last, err := fixture.service.ListUsers(ctx, fixture.actor, second[0].ID, 1)
	if err != nil || last == nil || len(last) != 0 {
		t.Fatal("空页未返回空数组", last, err)
	}
	nodes, err := fixture.service.ListNodes(ctx, fixture.actor, "", 100)
	if err != nil || len(nodes) != 2 {
		t.Fatal(nodes, err)
	}
	for _, node := range nodes {
		if node.Image == nil || node.Image.ID != fixture.imageID || !node.Image.IsEnabled || node.Image.Reference == "" {
			t.Fatal("节点目录未关联已验收镜像", node)
		}
		if node.ID == fixture.nodeID && (node.Capacity != 20 || node.Reserved != 1) {
			t.Fatal("节点容量未反映真实预留", node)
		}
	}
	if _, err := fixture.pool.Exec(`INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,state,error_code) VALUES($1,$2,'{}',$3,1,'quarantined','inspection_unconfirmed')`, fixture.nodeID, fixture.imageID, ids.New()); err != nil {
		t.Fatal(err)
	}
	nodes, err = fixture.service.ListNodes(ctx, fixture.actor, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range nodes {
		if node.ID == fixture.nodeID {
			found = true
			if node.VerificationStatus == nil || *node.VerificationStatus != "quarantined" {
				t.Fatal("列表未显示隔离验收状态", node)
			}
		}
	}
	if !found {
		t.Fatal("节点未出现在目录")
	}
	encoded, err := json.Marshal(struct {
		Users []UserSummary
		Nodes []NodeSummary
	}{first, nodes})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"endpoint", "container_id", "password", "token", "session_id", "volume_ref"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("目录泄露内部或认证字段", forbidden)
		}
	}
	for _, call := range []func(string, int) error{
		func(before string, limit int) error {
			_, err := fixture.service.ListUsers(ctx, fixture.actor, before, limit)
			return err
		},
		func(before string, limit int) error {
			_, err := fixture.service.ListNodes(ctx, fixture.actor, before, limit)
			return err
		},
	} {
		for _, page := range []struct {
			before string
			limit  int
		}{{"invalid", 1}, {"", 0}, {"", 101}} {
			if err := call(page.before, page.limit); !errors.Is(err, ErrInvalidInput) {
				t.Fatal("非法分页未拒绝", err)
			}
		}
		fixture.authorizer.denied = true
		if err := call("", 1); !errors.Is(err, identity.ErrForbidden) {
			t.Fatal("管理员撤权未生效", err)
		}
		fixture.authorizer.denied = false
	}
	ordinary := identity.Principal{UserID: ids.New(), Role: "user", Scope: "desktop"}
	if _, err := fixture.service.ListUsers(ctx, ordinary, "", 10); !errors.Is(err, identity.ErrForbidden) {
		t.Fatal("普通用户可列出账户", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fixture.service.ListUsers(canceled, fixture.actor, "", 10); err == nil {
		t.Fatal("取消查询被报告成功")
	}
}

func TestNodeCatalogBindsVerifiedImageAndPreservesHistory(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t), true)
	currentID, historicalID := ids.New(), ids.New()
	currentReference := "registry.example.test/current@sha256:" + strings.Repeat("a", 64)
	execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2),($3,$4)`, currentID, currentReference, historicalID, "registry.example.test/history@sha256:"+strings.Repeat("b", 64))
	readNode := func() NodeSummary {
		t.Helper()
		nodes, err := fixture.service.ListNodes(t.Context(), fixture.actor, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range nodes {
			if node.ID == fixture.nodeID {
				return node
			}
		}
		t.Fatal("节点未出现在目录")
		return NodeSummary{}
	}
	execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET status='bootstrapping',verified_image_id=NULL,capability_expires_at=NULL WHERE id=$1`, fixture.nodeID)
	if node := readNode(); node.Image != nil || node.Status != "bootstrapping" {
		t.Fatal("升级未验收时仍提供历史镜像", node)
	}
	execute(t, fixture.pool, `UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=clock_timestamp()+interval '15 minutes' WHERE id=$1`, fixture.nodeID, currentID)
	if node := readNode(); node.Image == nil || node.Image.ID != currentID || node.Image.Reference != currentReference || !node.Image.IsEnabled {
		t.Fatal("未切换为当前节点已验收镜像", node)
	}
	// 页面读取后节点可能升级，提交旧镜像仍须由开户事务拒绝，不占用容量。
	if _, err := fixture.service.reserve(t.Context(), fixture.actor.UserID, fixture.request()); !errors.Is(err, ErrNoCapacity) {
		t.Fatal("旧镜像快照仍可创建电脑", err)
	}
	execute(t, fixture.pool, `UPDATE tombot.computer_images SET is_enabled=false WHERE id=$1`, currentID)
	if node := readNode(); node.Image == nil || node.Image.IsEnabled {
		t.Fatal("未返回当前镜像停用状态", node)
	}
	var count, reserved int
	if err := fixture.pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.computer_images),reserved FROM tombot.computer_nodes WHERE id=$1`, fixture.nodeID).Scan(&count, &reserved); err != nil || count != 3 || reserved != 0 {
		t.Fatal("历史记录被修改或失效请求占用容量", count, reserved, err)
	}
}
