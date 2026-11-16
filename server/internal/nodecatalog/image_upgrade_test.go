package nodecatalog

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestImageUpgradeCleansQuarantinedResourceBeforeNewVerification(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	control := catalogDatabase(t, cluster, name, postgres.ControlRole)
	verifier, node := verifierFixture(t, control)
	processVerification(t, verifier)
	oldID := verifier.options.ImageID
	execute(t, pool, `UPDATE tombot.node_verifications SET state='quarantined' WHERE node_id=$1`, verifier.service.nodeID)
	execute(t, pool, `INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id) VALUES($1,'https://node.example.test:9444',$2,$3)`, verifier.service.nodeID, strings.Repeat("b", 64), oldID)
	registration := Registration{Name: "升级后的节点", Capacity: 1, PreviousImageID: oldID, ImageID: ids.New(), ImageReference: "registry.example.test/new@sha256:" + strings.Repeat("c", 64)}
	if err := control.Platform(t.Context(), func(tx *sql.Tx) error {
		if err := verifier.service.UpgradeImageInTransaction(t.Context(), tx, registration); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE tombot.node_connections SET image_id=$2 WHERE node_id=$1`, verifier.service.nodeID, registration.ImageID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if progressed, err := verifier.ProcessNext(t.Context()); err != nil || progressed {
		t.Fatal("旧监督器继续验收", progressed, err)
	}
	verifier.options.ImageID = registration.ImageID
	// 即使旧浏览器检查已不可用，清理也必须调用原身份的 Remove，不要求旧镜像通过新探针。
	node.inspectionError = context.DeadlineExceeded
	processVerification(t, verifier)
	if node.present || node.removeCalls != 1 || node.ensureCalls != 1 {
		t.Fatal("升级未先清理旧电脑", node.present, node.removeCalls, node.ensureCalls)
	}
	node.inspectionError = nil
	makeVerificationDue(t, pool, verifier)
	processVerification(t, verifier)
	processVerification(t, verifier)
	catalog, err := verifier.service.Get(t.Context(), registration.ImageID)
	if err != nil || catalog.Node.Status != "ready" || node.ensureCalls != 2 || node.removeCalls != 2 {
		t.Fatal("新镜像未完成验收与回收", catalog, err)
	}
	for _, spec := range node.identities {
		if spec.Generation == 1 && spec.ImageReference == registration.ImageReference {
			t.Fatal("覆盖了旧资源规格")
		}
		if spec.Generation == 2 && spec.ImageReference != registration.ImageReference {
			t.Fatal("仍使用旧镜像")
		}
	}
}
