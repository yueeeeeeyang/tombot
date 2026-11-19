package computerupgrade

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

func TestRetryQuarantinedUpgrade(t *testing.T) {
	pool, database, connection := upgradeDatabase(t)
	source, target := ids.New(), ids.New()
	for _, id := range []string{source, target} {
		execute(t, pool, `INSERT INTO tombot.computer_images(id,reference) VALUES($1,$2)`, id, "example.test/computer@sha256:"+secure.Hash(id))
	}
	userID, computerID, nodeID, _ := seedComputer(t, pool, source, target)
	execute(t, pool, `UPDATE tombot.node_connections SET supports_computer_upgrades=true WHERE node_id=$1`, nodeID)
	execute(t, pool, `UPDATE tombot.computer_nodes SET verified_image_id=$2 WHERE id=$1`, nodeID, target)
	node := &upgradeNode{}
	service := New(database, node, nil, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	process := func() {
		t.Helper()
		execute(t, pool, `UPDATE tombot.computer_image_upgrades SET next_attempt_at=clock_timestamp() WHERE user_id=$1`, userID)
		if progress, err := service.process(t.Context()); !progress || err != nil {
			t.Fatal("原升级计划未推进", err)
		}
	}
	process()
	for range maxAttempts {
		process()
	}
	checkPlan(t, pool, userID, "quarantined")
	last := node.requests[maxAttempts-1]
	query := func(script, containerID string, fence int64, operation string, wantSuccess bool) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		psql := filepath.Join(os.Getenv("TOM_TEST_POSTGRES_BIN"), "psql")
		command := exec.CommandContext(ctx, psql, "-X", "-Atq", "-w", "-v", "ON_ERROR_STOP=1", "-d", connection, "-v", "user_id="+userID, "-v", "computer_id="+computerID,
			"-v", "source_container_id="+containerID, "-v", "target_fence="+strconv.FormatInt(fence, 10),
			"-v", "operation_id="+operation, "-v", "node_id="+nodeID, "-v", "image_id="+target,
			"-f", "-")
		command.Stdin = strings.NewReader(script)
		output, err := command.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("原计划定向修复结果不符，%v，%s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	readRepair := func(name string) string {
		t.Helper()
		script, err := os.ReadFile(filepath.Join("../../deploy/repairs", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(script)
	}
	checkStatus := func(expected string) {
		t.Helper()
		if actual := query(historicalRecoveryStatusSQL, last.CurrentContainerID, last.Target.FencingToken, "", true); actual != expected {
			t.Fatalf("修复包读到的计划状态为 %q，期望 %q", actual, expected)
		}
	}
	checkStatus("retry")
	operation := ids.New()
	hold := func(operation string, ok bool) string {
		return query(readRepair("2026111902_hold_computer_recovery.sql"), last.CurrentContainerID, last.Target.FencingToken, operation, ok)
	}
	hold(operation, false)
	execute(t, pool, `UPDATE tombot.computer_nodes SET reserved=1 WHERE id=$1`, nodeID)
	execute(t, pool, `INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,state,lease_expires_at)
		VALUES($1,$2,'{}',$1,1,'checking',clock_timestamp()+interval '1 minute')`, nodeID, target)
	if reply := hold(operation, true); reply != "verification_busy" {
		t.Fatal("在途验收期间未返回等待回执", reply)
	}
	execute(t, pool, `UPDATE tombot.node_verifications SET lease_expires_at=NULL WHERE node_id=$1`, nodeID)
	if reply := hold(operation, true); reply != "" {
		t.Fatal("未取得维护窗口", reply)
	}
	hold(operation, true)
	hold(ids.New(), false)
	// 节点启动后由签名回执交还相同窗口，验收工作进程重新发布目标镜像能力。
	execute(t, pool, `UPDATE tombot.node_upgrades SET is_finished=true WHERE node_id=$1 AND operation_id=$2`, nodeID, operation)
	execute(t, pool, `UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=clock_timestamp()+interval '15 minutes' WHERE id=$1`, nodeID, target)
	repair := func(containerID string, fence int64, ok bool) {
		query(readRepair("2026111901_retry_interrupted_computer_upgrade.sql"), containerID, fence, operation, ok)
	}
	repair(secure.Hash("other"), last.Target.FencingToken, false)
	repair(last.CurrentContainerID, last.Target.FencingToken-1, false)
	checkPlan(t, pool, userID, "quarantined")
	repair(last.CurrentContainerID, last.Target.FencingToken, true)
	repair(last.CurrentContainerID, last.Target.FencingToken, false)
	checkPlan(t, pool, userID, "draining")
	checkStatus("draining")
	if current := readComputer(t, pool, userID); current.status != "stopping" || current.containerID != last.CurrentContainerID {
		t.Fatal("定向修复提前开放或更换了电脑", current)
	}
	process()
	checkStatus("switching")
	node.filesReady = true
	process()
	checkPlan(t, pool, userID, "succeeded")
	checkStatus("ready")
	if len(node.requests) != maxAttempts+1 || node.requests[maxAttempts].Target.FencingToken <= last.Target.FencingToken {
		t.Fatal("重新领取未推进原计划的控制版本")
	}
	var audits int
	if err := pool.QueryRow(`SELECT count(*) FROM tombot.admin_audit_events WHERE target_id=$1 AND action='重新排队已隔离的电脑镜像升级'`, computerID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("重新排队的审计记录数不符", audits, err)
	}
}

// 历史包和已发出的只读状态查询一起回归，确保前向迁移后仍可核验旧事务。
const historicalRecoveryStatusSQL = `SELECT CASE
 WHEN p.state='succeeded' AND c.status='ready' AND c.generation=p.source_generation+1
      AND c.image_id=p.target_image_id AND c.container_id<>p.source_container_id THEN 'ready'
 WHEN c.status<>'stopping' OR c.generation<>p.source_generation OR c.image_id<>p.source_image_id
      OR c.container_id IS DISTINCT FROM p.source_container_id THEN 'mismatch'
 WHEN p.state='quarantined' AND p.attempt_count>0 AND p.lease_expires_at IS NULL
      AND p.target_specification->>'fencing_token'=:'target_fence' THEN 'retry'
 ELSE p.state END
FROM tombot.computer_image_upgrades p
JOIN tombot.computers c ON (c.user_id,c.id,c.node_id)=(p.user_id,p.computer_id,p.node_id)
JOIN tombot.node_connections n ON n.node_id=p.node_id AND n.image_id=p.target_image_id AND n.supports_computer_upgrades
WHERE p.node_id=:'node_id' AND p.user_id=:'user_id' AND p.computer_id=:'computer_id' AND p.source_container_id=:'source_container_id'
 AND p.target_image_id=:'image_id';`
