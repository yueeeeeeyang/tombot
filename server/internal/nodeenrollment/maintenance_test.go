package nodeenrollment

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func TestMaintenanceWaitsForExistingComputerImageUpgrades(t *testing.T) {
	service, pool, actor, _ := serviceFixture(t, 1)
	ctx := t.Context()
	pair := certificate(t)
	source := claimFor(pair)
	source.SupportsComputerUpgrades = true
	grant, err := service.Issue(ctx, actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Register(ctx, signed(t, pair, source), grant.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	register := func(claim Claim, state string) {
		t.Helper()
		receipt, err := service.Register(ctx, signed(t, pair, claim), "", ids.New())
		if err != nil || receipt.State != state {
			t.Fatalf("节点登记状态为 %q，预期 %q：%v", receipt.State, state, err)
		}
	}
	execute := func(query string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, query, arguments...); err != nil {
			t.Fatal(err)
		}
	}

	target := source
	target.Registration.PreviousImageID = source.Registration.ImageID
	target.Registration.ImageID = ids.New()
	target.Registration.ImageReference = "registry.example.test/computer@sha256:" + strings.Repeat("b", 64)
	operationID := ids.New()
	target.Maintenance = &MaintenanceRequest{OperationID: operationID}
	register(target, "upgrade_held")
	target.Maintenance = nil
	register(target, "registered")

	userID, computerID := ids.New(), ids.New()
	containerID := strings.Repeat("a", 64)
	execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,'migration-user','迁移测试用户','provisioning')`, userID)
	execute(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
		VALUES($1,$2,$3,$4,'stopping',$5,$6,1000,1073741824,1073741824,128)`,
		userID, computerID, source.NodeID, source.Registration.ImageID, containerID, "tombot-user-"+userID)
	execute(`INSERT INTO tombot.computer_image_upgrades(user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,
		source_container_id,source_specification,target_specification,state)
		VALUES($1,$2,$3,$4,$5,1,$6,'{}','{}','draining')`,
		userID, computerID, source.NodeID, source.Registration.ImageID, target.Registration.ImageID, containerID)

	target.Maintenance = &MaintenanceRequest{OperationID: operationID}
	register(target, "upgrade_held")
	target.Maintenance = &MaintenanceRequest{OperationID: operationID, Finish: true}
	register(target, "upgrade_finished")
	register(target, "upgrade_finished")

	next := target
	next.Registration.PreviousImageID = target.Registration.ImageID
	next.Registration.ImageID = ids.New()
	next.Registration.ImageReference = "registry.example.test/computer@sha256:" + strings.Repeat("c", 64)
	next.Maintenance = &MaintenanceRequest{OperationID: ids.New()}
	for _, state := range []string{"draining", "switching", "quarantined"} {
		t.Run(state, func(t *testing.T) {
			execute(`UPDATE tombot.computer_image_upgrades SET state=$2 WHERE user_id=$1`, userID, state)
			if _, err := service.Register(ctx, signed(t, pair, next), "", ids.New()); !errors.Is(err, ErrConflict) {
				t.Fatalf("原电脑迁移处于 %s 时取得下一次维护：%v", state, err)
			}
			var retainedOperation, retainedTarget string
			var finished bool
			if err := pool.QueryRowContext(ctx, `SELECT operation_id,target_image_id,is_finished FROM tombot.node_upgrades WHERE node_id=$1`, source.NodeID).
				Scan(&retainedOperation, &retainedTarget, &finished); err != nil {
				t.Fatal(err)
			}
			if retainedOperation != operationID || retainedTarget != target.Registration.ImageID || !finished {
				t.Fatal("冲突请求改写了已完成的维护记录")
			}
		})
	}
	execute(`UPDATE tombot.computer_image_upgrades SET state='succeeded' WHERE user_id=$1`, userID)
	register(next, "upgrade_held")
}

// 验收被隔离时宿主维护仍绑定原镜像和发布，只能在电脑迁移尚未取得租约时交接。
func TestQuarantinedVerificationHostRepair(t *testing.T) {
	s, db, actor, _ := serviceFixture(t, 1)
	ctx := t.Context()
	pair := certificate(t)
	c := claimFor(pair)
	c.SupportsComputerUpgrades = true
	g, err := s.Issue(ctx, actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Register(ctx, signed(t, pair, c), g.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	send := func(m MaintenanceRequest, want string) {
		t.Helper()
		c.Maintenance = &m
		r, err := s.Register(ctx, signed(t, pair, c), "", ids.New())
		if want == "" {
			if !errors.Is(err, ErrConflict) {
				t.Fatal("应拒绝冲突请求", r, err)
			}
			return
		}
		if err != nil || r.State != want {
			t.Fatal(want, r, err)
		}
	}
	image, old, user, id := c.Registration.ImageID, ids.New(), ids.New(), ids.New()
	exec(`INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, old, "registry.example.test/old@sha256:"+strings.Repeat("d", 64))
	exec(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,lower($1),'升级测试','provisioning')`, user)
	exec(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
		VALUES($1,$2,$3,$4,'stopping',$5,$6,1000,1073741824,1073741824,128)`, user, id, c.NodeID, old, strings.Repeat("b", 64), "tombot-user-"+user)
	exec(`INSERT INTO tombot.computer_image_upgrades(user_id,computer_id,node_id,source_image_id,target_image_id,source_generation,source_container_id,source_specification,target_specification,state)
		VALUES($1,$2,$3,$4,$5,1,$6,'{}','{}','draining')`, user, id, c.NodeID, old, image, strings.Repeat("b", 64))
	exec(`INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,state,node_error_code,fencing_token,is_valid)
		VALUES($1,$2,'{}',$1,1,'quarantined','agent_retirement_entry_unknown',17,false)`, c.NodeID, image)
	release := strings.Repeat("a", 64)
	c.Maintenance = &MaintenanceRequest{InspectRelease: release}
	r, err := s.Register(ctx, signed(t, pair, c), "", ids.New())
	if err != nil || r.Recovery == nil || r.Recovery.State != "blocked" || r.Recovery.ErrorCode != "verification_quarantined/agent_retirement_entry_unknown" {
		t.Fatal(r, err)
	}
	m := MaintenanceRequest{OperationID: ids.New(), HostRelease: release}
	send(MaintenanceRequest{OperationID: ids.New()}, "")
	exec(`UPDATE tombot.computer_image_upgrades SET state='switching' WHERE user_id=$1`, user)
	send(m, "")
	exec(`UPDATE tombot.computer_image_upgrades SET state='draining',lease_expires_at=clock_timestamp()+interval '1 second' WHERE user_id=$1`, user)
	send(m, "")
	exec(`UPDATE tombot.computer_image_upgrades SET lease_expires_at=NULL WHERE user_id=$1`, user)
	// 模拟刚进入会话准入而尚未插入外键的事务。等待维护争锁后，它仍须能立即锁定节点。
	admission, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admission.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	})
	var pid int
	if err = admission.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM pg_advisory_xact_lock_shared(hashtextextended('tombot.node-maintenance/'||$1,0))`, c.NodeID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	claim := c
	claim.Maintenance = &m
	request := signed(t, pair, claim)
	held := make(chan error, 1)
	go func() { _, err := s.Register(ctx, request, "", ids.New()); held <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks l WHERE l.locktype='advisory' AND NOT l.granted AND $1=ANY(pg_blocking_pids(l.pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("宿主维护没有等待会话准入锁")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = admission.ExecContext(ctx, `SELECT id FROM tombot.computer_nodes WHERE id=$1 FOR KEY SHARE NOWAIT`, c.NodeID); err != nil {
		t.Fatal("维护阻塞了先进入会话的外键检查", err)
	}
	if err = admission.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-held; err != nil {
		t.Fatal(err)
	}
	send(m, "upgrade_held")
	wrong := m
	wrong.HostRelease = strings.Repeat("e", 64)
	wrong.Finish = true
	send(wrong, "")
	m.Stage = "start"
	send(m, "upgrade_progress")
	var stage string
	if err := db.QueryRowContext(ctx, `SELECT tombot.node_upgrade_diagnosis($1)`, c.NodeID).Scan(&stage); err != nil || stage != "start" {
		t.Fatal(stage, err)
	}
	m.Stage = ""
	m.Finish = true
	send(m, "upgrade_finished")
	send(m, "upgrade_finished")
	verify := func(want string) {
		t.Helper()
		var state string
		var fence int
		var valid bool
		if err := db.QueryRowContext(ctx, `SELECT state,fencing_token,is_valid FROM tombot.node_verifications WHERE node_id=$1`, c.NodeID).Scan(&state, &fence, &valid); err != nil || state != want || valid || fence != 17 {
			t.Fatal(state, fence, valid, err)
		}
	}
	verify("cleaning")
	exec(`UPDATE tombot.node_verifications SET state='quarantined' WHERE node_id=$1`, c.NodeID)
	m.OperationID = ids.New()
	m.Finish = false
	send(m, "upgrade_held")
	m.Finish = true
	send(m, "upgrade_finished")
	verify("quarantined")
	m.OperationID = ids.New()
	m.HostRelease = strings.Repeat("c", 64)
	m.Finish = false
	send(m, "upgrade_held")
	m.Stage = "rolled_back"
	send(m, "upgrade_progress")
	send(m, "upgrade_progress")
	verify("quarantined")
	m.Stage = ""
	m.Finish = true
	send(m, "")
}
