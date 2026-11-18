package nodeenrollment

import (
	"errors"
	"strings"
	"testing"

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
