package nodeenrollment

import (
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// 标准升级以已登记节点的签名，验证跨节点隔离、过期控制版本和回执重放。
func TestSignedStandardUpgradeRecoversOnlyBoundPlan(t *testing.T) {
	for _, code := range []string{"computer_image_upgrade_source_mismatch", "container_stop_unconfirmed"} {
		t.Run(code, func(t *testing.T) { checkBoundRecovery(t, code) })
	}
}

func checkBoundRecovery(t *testing.T, code string) {
	t.Helper()
	s, pool, actor, _ := serviceFixture(t, 2)
	ctx := t.Context()
	pair := certificate(t)
	claim := claimFor(pair)
	claim.SupportsComputerUpgrades = true
	grant, err := s.Issue(ctx, actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Register(ctx, signed(t, pair, claim), grant.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	query := func(m MaintenanceRequest) (Receipt, error) {
		c := claim
		c.Maintenance = &m
		return s.Register(ctx, signed(t, pair, c), "", ids.New())
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	image := claim.Registration.ImageID
	old, user, id := ids.New(), ids.New(), ids.New()
	src := computer.Specification{OperationID: ids.New(), UserID: user, ComputerID: id, NodeID: claim.NodeID, ImageReference: "example.test/computer@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + user, Generation: 1, Attempt: 1, FencingToken: 2, LeaseExpiresAt: time.Now().UTC(), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 128}}
	container := strings.Repeat("e", 64)
	release := strings.Repeat("b", 64)
	inspect := func(want string) *RecoveryInspection {
		t.Helper()
		receipt, err := query(MaintenanceRequest{InspectRelease: release})
		if err != nil || receipt.Recovery == nil || receipt.Recovery.State != want {
			t.Fatal(want, receipt, err)
		}
		return receipt.Recovery
	}
	inspect("clear")
	exec(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, old, src.ImageReference)
	exec(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,lower($1),'测试用户','provisioning')`, user)
	exec(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
	VALUES($1,$2,$3,$4,'stopping',$5,$6,1000,1073741824,1073741824,128)`, user, id, claim.NodeID, old, container, src.VolumeReference)
	exec(`INSERT INTO tombot.computer_execution_heads(user_id,computer_id) VALUES($1,$2)`, user, id)
	exec(`INSERT INTO tombot.computer_image_upgrades(user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,source_container_id,source_specification,target_specification,state,attempt_count)
	VALUES($1,$2,$3,$4,$5,1,$6,$7,'{"fencing_token":10}','quarantined',5)`, user, id, claim.NodeID, old, image, container, src)
	exec(`INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,state,lease_expires_at)
		VALUES($1,$2,'{}',$1,1,'checking',clock_timestamp()+interval '2 minutes')`, claim.NodeID, image)
	inspect("waiting")
	exec(`UPDATE tombot.node_verifications SET lease_expires_at=NULL WHERE node_id=$1`, claim.NodeID)
	target := inspect("retry").Target
	op := ids.New()
	hold := func(proof RecoveryTarget) error {
		_, err := query(MaintenanceRequest{OperationID: op, Recovery: &proof})
		return err
	}
	stale := target
	stale.Fence--
	if err := hold(stale); !errors.Is(err, ErrConflict) {
		t.Fatal("旧控制版本取得了窗口", err)
	}
	exec(`UPDATE tombot.computer_image_upgrades SET last_failure_code='workspace_not_ready' WHERE user_id=$1`, user)
	inspect("blocked")
	if err := hold(target); !errors.Is(err, ErrConflict) {
		t.Fatal("其他故障被自动重试", err)
	}
	exec(`UPDATE tombot.computer_image_upgrades SET last_failure_code=$2 WHERE user_id=$1`, user, code)
	if check := inspect("retry"); check.ErrorCode != code || check.Target != target {
		t.Fatal("恢复窗口未绑定原故障与控制版本", check)
	}
	other := claim
	other.NodeID = ids.New()
	other.Maintenance = &MaintenanceRequest{InspectRelease: release}
	if _, err := s.Register(ctx, signed(t, pair, other), "", ids.New()); err == nil {
		t.Fatal("读取了未绑定节点的计划")
	}
	if err := hold(target); err != nil {
		t.Fatal(err)
	}
	if err := hold(target); err != nil {
		t.Fatal("同一维护回执重放失败", err)
	}
	inspect("waiting")
	if _, err := query(MaintenanceRequest{OperationID: op, Finish: true}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE tombot.computer_image_upgrades SET state='quarantined',attempt_count=5 WHERE user_id=$1`, user)
	inspect("blocked")
	op = ids.New()
	if err := hold(target); !errors.Is(err, ErrConflict) {
		t.Fatal("相同发布重复领取重试额度", err)
	}
	var audits int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND action='标准节点升级恢复隔离电脑'`, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("审计重复或缺失", audits, err)
	}
}
