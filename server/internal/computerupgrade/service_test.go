package computerupgrade

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type upgradeNode struct {
	requests     []computer.ImageUpgrade
	browserReady bool
}

func (node *upgradeNode) UpgradeComputer(_ context.Context, upgrade computer.ImageUpgrade) (computer.Observation, error) {
	node.requests = append(node.requests, upgrade)
	target := upgrade.Target
	return computer.Observation{
		OperationID: target.OperationID, UserID: target.UserID, ComputerID: target.ComputerID,
		Generation: target.Generation, FencingToken: target.FencingToken, VolumeReference: target.VolumeReference,
		State: "ready", ContainerID: secure.Hash(target.ImageReference),
		Capabilities: computer.Capabilities{Terminal: true, Browser: node.browserReady, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true},
	}, nil
}

type upgradeComputerRow struct {
	status, imageID, containerID string
	generation                   int64
}

func execute(t *testing.T, pool *sql.DB, query string, arguments ...any) {
	t.Helper()
	if _, err := pool.Exec(query, arguments...); err != nil {
		t.Fatal(err)
	}
}

func readComputer(t *testing.T, pool *sql.DB, userID string) upgradeComputerRow {
	t.Helper()
	var value upgradeComputerRow
	if err := pool.QueryRow(`SELECT status,image_id,container_id,generation FROM tombot.computers WHERE user_id=$1`, userID).
		Scan(&value.status, &value.imageID, &value.containerID, &value.generation); err != nil {
		t.Fatal(err)
	}
	return value
}

func checkPlan(t *testing.T, pool *sql.DB, userID, expected string) {
	t.Helper()
	var state string
	if err := pool.QueryRow(`SELECT state FROM tombot.computer_image_upgrades WHERE user_id=$1`, userID).Scan(&state); err != nil || state != expected {
		t.Fatal("电脑升级计划状态不符", state, err)
	}
}

func seedComputer(t *testing.T, pool *sql.DB, sourceImage, targetImage string) (userID, computerID, nodeID, operationID string) {
	t.Helper()
	userID, computerID, nodeID, operationID = ids.New(), ids.New(), ids.New(), ids.New()
	execute(t, pool, `INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'镜像升级用户','provisioning')`, userID, strings.ToLower(userID))
	execute(t, pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at)
		VALUES($1,$1,'ready',4,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, nodeID, sourceImage)
	execute(t, pool, `INSERT INTO tombot.node_connections(node_id,url,certificate_fingerprint,image_id)
		VALUES($1,$2,$3,$4)`, nodeID, "https://node-"+strings.ToLower(nodeID)+".example.test", secure.Hash(nodeID), targetImage)
	execute(t, pool, `INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
		VALUES($1,$2,$3,$4,'ready',$5,$6,1000,1073741824,1073741824,128)`, userID, computerID, nodeID, sourceImage, secure.Hash(computerID), "tombot-user-"+userID)
	execute(t, pool, `INSERT INTO tombot.user_creation_operations(id,user_id,computer_id,idempotency_key,request_digest,status,stage,attempt,fencing_token,deadline_at,request_id)
		VALUES($1,$2,$3,$1,decode($4,'hex'),'succeeded','succeeded',4,7,clock_timestamp()+interval '1 minute',$1)`, operationID, userID, computerID, secure.Hash(operationID))
	execute(t, pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, userID)
	return userID, computerID, nodeID, operationID
}

func upgradeDatabase(t *testing.T) (*sql.DB, *postgres.Database, string) {
	t.Helper()
	ctx := t.Context()
	cluster := pgtest.Start(t)
	pool, databaseName := cluster.Database(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	login := cluster.Login(t, string(postgres.ControlRole))
	database, err := postgres.Open(ctx, postgres.Options{DSN: cluster.DSN(databaseName, login), Role: postgres.ControlRole, MaxConnections: 4, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second, LockTimeout: time.Second, TransactionTimeout: 10 * time.Second, ConnectionLifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	var owner string
	if err := pool.QueryRow("SELECT current_user").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return pool, database, cluster.DSN(databaseName, owner)
}

func TestUpgradeExistingComputer(t *testing.T) {
	ctx := context.Background()
	pool, database, _ := upgradeDatabase(t)
	source, target, next := ids.New(), ids.New(), ids.New()
	imageReference := func(image string) string { return "example.test/computer@sha256:" + secure.Hash(image) }
	for _, image := range []string{source, target, next} {
		execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, image, imageReference(image))
	}
	userID, computerID, nodeID, operationID := seedComputer(t, pool, source, target)
	foreignUser, _, foreignNode, _ := seedComputer(t, pool, source, target)
	execute(t, pool, `UPDATE tombot.node_connections SET supports_computer_upgrades=true WHERE node_id=$1`, foreignNode)
	original, foreign := readComputer(t, pool, userID), readComputer(t, pool, foreignUser)
	node := &upgradeNode{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := New(database, node, nil, time.Second, logger)
	process := func(wantProgress bool) {
		t.Helper()
		if actual, err := service.process(ctx); err != nil || actual != wantProgress {
			t.Fatal("对账结果不符", actual, err)
		}
	}
	allowNext := func() {
		t.Helper()
		execute(t, pool, `UPDATE tombot.computer_image_upgrades SET next_attempt_at=clock_timestamp() WHERE user_id=$1`, userID)
	}

	execute(t, pool, `UPDATE tombot.computer_nodes SET verified_image_id=$2 WHERE id=$1`, nodeID, target)
	process(false)
	execute(t, pool, `UPDATE tombot.node_connections SET supports_computer_upgrades=true WHERE node_id=$1`, nodeID)
	execute(t, pool, `UPDATE tombot.computer_nodes SET capability_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, nodeID)
	process(false)
	execute(t, pool, `UPDATE tombot.computer_nodes SET capability_expires_at=clock_timestamp()+interval '15 minutes' WHERE id=$1`, nodeID)
	execute(t, pool, `INSERT INTO tombot.node_upgrades(node_id,operation_id,target_image_id) VALUES($1,$2,$3)`, nodeID, ids.New(), target)
	process(false)
	if len(node.requests) != 0 || readComputer(t, pool, userID) != original {
		t.Fatal("节点未满足就绪条件时启动了迁移")
	}
	execute(t, pool, `UPDATE tombot.node_upgrades SET is_finished=true WHERE node_id=$1`, nodeID)
	process(true)
	checkPlan(t, pool, userID, "switching")
	stopping := original
	stopping.status = "stopping"
	if len(node.requests) != 0 || readComputer(t, pool, userID) != stopping {
		t.Fatal("排空阶段提前更改了已有电脑实例")
	}

	allowNext()
	interrupted, err := service.claim(ctx)
	if err != nil || interrupted == nil {
		t.Fatal("未领取可恢复的切换租约", err)
	}
	service = New(database, node, nil, time.Second, logger)
	process(false)
	execute(t, pool, `UPDATE tombot.computer_image_upgrades SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, userID)
	process(true)
	checkPlan(t, pool, userID, "switching")
	if len(node.requests) != 1 || readComputer(t, pool, userID) != stopping {
		t.Fatal("不完整的节点回执更新了用户电脑")
	}
	first := node.requests[0]
	if first.Current.OperationID != operationID || first.Current.ComputerID != computerID || first.Current.NodeID != nodeID ||
		first.CurrentContainerID != original.containerID || first.Current.ImageReference != imageReference(source) ||
		first.Current.Generation != original.generation || first.Current.Attempt != 4 || first.Current.FencingToken != 7 ||
		first.Target.ImageReference != imageReference(target) || first.Target.VolumeReference != "tombot-user-"+userID ||
		first.Target.FencingToken <= interrupted.upgrade.Target.FencingToken || first.Validate() != nil {
		t.Fatal("恢复后切换请求遗漏原实例、磁盘或最新租约")
	}
	process(false)
	allowNext()
	node.browserReady = true
	process(true)
	checkPlan(t, pool, userID, "succeeded")
	if len(node.requests) != 2 {
		t.Fatal("切换未按持久计划重试")
	}
	previous := node.requests[1]
	committed := upgradeComputerRow{status: "ready", imageID: target, containerID: secure.Hash(imageReference(target)), generation: 2}
	if readComputer(t, pool, userID) != committed {
		t.Fatal("有效节点验收回执未提交")
	}

	execute(t, pool, `UPDATE tombot.node_connections SET image_id=$2 WHERE node_id=$1`, nodeID, next)
	process(false)
	execute(t, pool, `UPDATE tombot.computer_nodes SET verified_image_id=$2 WHERE id=$1`, nodeID, next)
	process(true)
	allowNext()
	process(true)
	checkPlan(t, pool, userID, "succeeded")
	if len(node.requests) != 3 {
		t.Fatal("二次迁移没有发送切换请求")
	}
	current := node.requests[2].Current
	current.LeaseExpiresAt = current.LeaseExpiresAt.UTC()
	previous.Target.LeaseExpiresAt = previous.Target.LeaseExpiresAt.UTC()
	if current != previous.Target || node.requests[2].CurrentContainerID != committed.containerID || node.requests[2].Target.Generation != 3 {
		t.Fatal("二次迁移未绑定上次验收通过的电脑实例")
	}
	if readComputer(t, pool, userID) != (upgradeComputerRow{status: "ready", imageID: next, containerID: secure.Hash(imageReference(next)), generation: 3}) || readComputer(t, pool, foreignUser) != foreign {
		t.Fatal("升级修改了其他节点电脑或未提交新代次")
	}
	var plans, audits int
	if err = pool.QueryRow(`SELECT count(*) FROM tombot.computer_image_upgrades`).Scan(&plans); err != nil || plans != 1 {
		t.Fatal("为无关电脑建立了升级计划", plans, err)
	}
	if err = pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND target_kind='computer'`, computerID).Scan(&audits); err != nil || audits != 2 {
		t.Fatal("有效切换缺少审计记录或错误回执提交了审计", audits, err)
	}
	process(false)
}
