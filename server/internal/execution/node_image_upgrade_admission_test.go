package execution

import (
	"testing"

	"io.yueyang/tom/internal/computercontrol"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestNodeImageUpgradeAdmissionBeforeComputerPlan(t *testing.T) {
	cluster := pgtest.Start(t)
	fixture := newFixture(t, cluster)
	ctx := t.Context()
	node := &sessionNode{}
	computers := fixture.computers(t, node, computerOptions())
	var nodeID, imageID string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT node_id,image_id FROM tombot.computers WHERE user_id=$1`, fixture.a.UserID).
		Scan(&nodeID, &imageID); err != nil {
		t.Fatal(err)
	}
	execute(t, fixture.pool, `INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id,supports_computer_upgrades)
		VALUES($1,'https://upgrade-node.example.test',$2,$3,true)`, nodeID, secure.Hash(nodeID), imageID)
	targetImage := ids.New()
	execute(t, fixture.pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`,
		targetImage, "registry.example.test/computer@sha256:"+secure.Hash(targetImage))
	execute(t, fixture.pool, `INSERT INTO tombot.node_upgrades(node_id,operation_id,target_image_id) VALUES($1,$2,$3)`,
		nodeID, ids.New(), targetImage)
	fixture.send(t, fixture.a, fixture.first)
	lease := fixture.claim(t)
	fixture.start(t, lease)
	reference := computerReference(lease)
	expectBlocked := func() {
		t.Helper()
		_, err := computers.Acquire(ctx, reference)
		expectImageUpgradeWait(t, err)
		var count int
		if err = fixture.pool.QueryRowContext(ctx, `SELECT count(*) FROM tombot.computer_sessions WHERE user_id=$1`, lease.UserID).Scan(&count); err != nil || count != 0 || node.opens != 0 {
			t.Fatal("维护或镜像切换期间签发了旧实例会话", count, err)
		}
	}
	expectBlocked()
	execute(t, fixture.pool, `UPDATE tombot.node_upgrades SET is_finished=true WHERE node_id=$1`, nodeID)
	execute(t, fixture.pool, `UPDATE tombot.node_connections SET image_id=$2 WHERE node_id=$1`, nodeID, targetImage)
	expectBlocked()
	execute(t, fixture.pool, `INSERT INTO tombot.computer_image_upgrades(user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,source_container_id,source_specification,target_specification,state)
		SELECT user_id,id,node_id,image_id,$2,generation,container_id,'{}','{}','quarantined' FROM tombot.computers WHERE user_id=$1`,
		lease.UserID, targetImage)
	execute(t, fixture.pool, `UPDATE tombot.computers SET status='quarantined' WHERE user_id=$1`, lease.UserID)
	_, err := computers.Acquire(ctx, reference)
	expect(t, err, computercontrol.ErrUnavailable)
	containerID := secure.Hash("已验收的目标容器")
	execute(t, fixture.pool, `UPDATE tombot.computers SET status='ready',generation=generation+1,image_id=$2,container_id=$3 WHERE user_id=$1`,
		lease.UserID, targetImage, containerID)
	execute(t, fixture.pool, `UPDATE tombot.computer_image_upgrades SET state='succeeded' WHERE user_id=$1`, lease.UserID)
	session, err := computers.Acquire(ctx, reference)
	if err != nil || session.ContainerID != containerID || node.opens != 1 {
		t.Fatal("迁移结束后未绑定目标容器", err)
	}
}

func TestNodeMaintenanceKeepsWaitingAndActiveSessions(t *testing.T) {
	cluster := pgtest.Start(t)
	fixture := newFixture(t, cluster)
	ctx := t.Context()
	node := &sessionNode{}
	computers := fixture.computers(t, node, computerOptions())
	execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_desktops=1 WHERE user_id=$1`, fixture.a.UserID)
	fixture.send(t, fixture.a, fixture.first)
	first := fixture.claim(t)
	fixture.start(t, first)
	fixture.send(t, fixture.a, fixture.second)
	second := fixture.claim(t)
	fixture.start(t, second)
	active, err := computers.Acquire(ctx, computerReference(first))
	if err != nil {
		t.Fatal(err)
	}
	_, err = computers.Acquire(ctx, computerReference(second))
	expect(t, err, computercontrol.ErrWaiting)
	execute(t, fixture.pool, `INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id,supports_computer_upgrades)
		SELECT node_id,'https://upgrade-node.example.test',$2,image_id,true FROM tombot.computers WHERE user_id=$1`,
		first.UserID, secure.Hash(active.NodeID))
	execute(t, fixture.pool, `INSERT INTO tombot.node_upgrades(node_id,operation_id,target_image_id)
		SELECT node_id,$2,image_id FROM tombot.computers WHERE user_id=$1`, first.UserID, ids.New())
	_, err = computers.Acquire(ctx, computerReference(second))
	expectImageUpgradeWait(t, err)
	if fixture.sessionState(t, second) != "waiting" || node.opens != 1 {
		t.Fatal("维护改变了旧实例排队项或绕过了准入")
	}
	session, err := computers.Acquire(ctx, computerReference(first))
	if err != nil || session != active {
		t.Fatal("维护中断了已授权的会话", err)
	}
	if err = computers.Release(ctx, computerReference(first)); err != nil || node.closes != 1 {
		t.Fatal("维护期间无法正常排空原会话", err)
	}
	execute(t, fixture.pool, `UPDATE tombot.node_upgrades SET is_finished=true WHERE node_id=$1`, active.NodeID)
	if _, err = computers.Acquire(ctx, computerReference(second)); err != nil || node.opens != 2 {
		t.Fatal("同镜像维护完成后未恢复既有排队项", err)
	}
}
