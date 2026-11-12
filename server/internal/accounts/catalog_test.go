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
		if node.ID == fixture.nodeID && (node.Capacity != 20 || node.Reserved != 1) {
			t.Fatal("节点容量未反映真实预留", node)
		}
	}
	images, err := fixture.service.ListImages(ctx, fixture.actor, "", 100)
	if err != nil || len(images) != 1 || images[0].ID != fixture.imageID || !images[0].IsEnabled {
		t.Fatal(images, err)
	}
	encoded, err := json.Marshal(struct {
		Users  []UserSummary
		Nodes  []NodeSummary
		Images []ImageSummary
	}{first, nodes, images})
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
		func(before string, limit int) error {
			_, err := fixture.service.ListImages(ctx, fixture.actor, before, limit)
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
