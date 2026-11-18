package nodeenrollment

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
)

type fleetUpgradeLog struct {
	mu   sync.Mutex
	body strings.Builder
}

func (log *fleetUpgradeLog) Write(encoded []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.body.Write(encoded)
}
func (log *fleetUpgradeLog) has(value string) bool {
	log.mu.Lock()
	defer log.mu.Unlock()
	return strings.Contains(log.body.String(), value)
}

func TestFleetChangesVerifierAfterSignedImageUpgrade(t *testing.T) {
	service, _, actor, _ := serviceFixture(t, 1)
	grant, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	pair := certificate(t)
	claim := claimFor(pair)
	if _, err = service.Register(t.Context(), signed(t, pair, claim), grant.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	log := &fleetUpgradeLog{}
	fleet, err := NewFleet(service, service.control, &unavailableRuntime{}, nodecatalog.ObserverOptions{CallTimeout: time.Second, Interval: time.Second, Lifetime: time.Minute}, nodecatalog.VerificationOptions{Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30, ProcessLimit: 128}, CallTimeout: time.Second, LeaseDuration: 10 * time.Second, Interval: time.Minute, Lifetime: 2 * time.Minute, RetryDelay: time.Second, MaxAttempts: 3}, 1, slog.New(slog.NewTextHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fleet.Run(ctx) }()
	wait := func(imageID string) {
		t.Helper()
		deadline := time.NewTimer(4 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for !log.has("image_id=" + imageID) {
			select {
			case <-deadline.C:
				t.Fatal("未切换镜像监督", imageID)
			case err := <-done:
				t.Fatal("监督提前退出", err)
			case <-tick.C:
			}
		}
	}
	wait(claim.Registration.ImageID)
	claim.Registration.PreviousImageID = claim.Registration.ImageID
	claim.Registration.ImageID = ids.New()
	claim.Registration.ImageReference = "registry.example.test/next@sha256:" + strings.Repeat("c", 64)
	if _, err = service.Register(t.Context(), signed(t, pair, claim), "", ids.New()); err != nil {
		t.Fatal(err)
	}
	wait(claim.Registration.ImageID)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("镜像监督未释放")
	}
}

func TestSignedNodeImageUpgrade(t *testing.T) {
	service, pool, actor, _ := serviceFixture(t, 1)
	grant, err := service.Issue(t.Context(), actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	pair := certificate(t)
	initial := claimFor(pair)
	if _, err = service.Register(t.Context(), signed(t, pair, initial), grant.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	upgrade := initial
	upgrade.Registration.PreviousImageID = initial.Registration.ImageID
	upgrade.Registration.ImageID = ids.New()
	upgrade.Registration.ImageReference = "registry.example.test/computer@sha256:" + strings.Repeat("b", 64)
	if _, err := pool.Exec(`UPDATE tombot.computer_nodes SET reserved=1 WHERE id=$1`, initial.NodeID); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"unsigned_transition", "wrong_previous", "reserved", "retry", "replay_old"} {
		t.Run(scenario, func(t *testing.T) {
			claim := upgrade
			switch scenario {
			case "unsigned_transition":
				claim.Registration.PreviousImageID = ""
			case "wrong_previous":
				claim.Registration.PreviousImageID = ids.New()
			case "replay_old":
				claim = initial
			}
			_, err := service.Register(t.Context(), signed(t, pair, claim), "", ids.New())
			if (err == nil) != (scenario == "reserved" || scenario == "retry") {
				t.Fatal(scenario, err)
			}
		})
	}
	endpoint, err := service.Resolve(t.Context(), initial.NodeID)
	if err != nil || endpoint.ImageID != upgrade.Registration.ImageID {
		t.Fatal(endpoint, err)
	}
	var reference, status string
	if err = pool.QueryRow(`SELECT reference FROM tombot.computer_images WHERE id=$1`, initial.Registration.ImageID).Scan(&reference); err != nil || reference != initial.Registration.ImageReference {
		t.Fatal("旧镜像被改写", reference, err)
	}
	if err = pool.QueryRow(`SELECT status FROM tombot.computer_nodes WHERE id=$1`, initial.NodeID).Scan(&status); err != nil || status != "bootstrapping" {
		t.Fatal("升级跳过能力验收", status, err)
	}
}

func TestMaintenanceFenceAndReplay(t *testing.T) {
	s, pool, actor, _ := serviceFixture(t, 1)
	ctx := t.Context()
	g, err := s.Issue(ctx, actor, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	p := certificate(t)
	c := claimFor(p)
	if _, err = s.Register(ctx, signed(t, p, c), g.Token, ids.New()); err != nil {
		t.Fatal(err)
	}
	oldImage := c.Registration.ImageID
	userID, computerID := ids.New(), ids.New()
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`UPDATE tombot.computer_nodes SET runtime_id=$2,generation=1,control_sequence=1,last_seen_at=clock_timestamp(),control_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, c.NodeID, ids.New())
	execute(`UPDATE tombot.computer_nodes SET status='ready',verified_image_id=$2,capability_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, c.NodeID, oldImage)
	reserve := func() error {
		var nodeID string
		return pool.QueryRowContext(ctx, `SELECT tombot.reserve_computer_node($1,$2)`, c.NodeID, oldImage).Scan(&nodeID)
	}
	if err := reserve(); err != nil {
		t.Fatal("未能预留初始电脑", err)
	}
	execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,'upgrade-user','升级测试用户','provisioning')`, userID)
	execute(`INSERT INTO tombot.computers(user_id,id,node_id,image_id,status,container_id,volume_ref,cpu_millis,memory_bytes,disk_bytes,process_limit)
		VALUES($1,$2,$3,$4,'ready',$5,$6,1000,1073741824,1073741824,128)`, userID, computerID, c.NodeID, oldImage, strings.Repeat("a", 64), "tombot-user-"+userID)
	execute(`UPDATE tombot.users SET status='pending_activation' WHERE id=$1`, userID)
	readComputer := func() string {
		t.Helper()
		var record string
		if err := pool.QueryRowContext(ctx, `SELECT to_jsonb(c)::text FROM tombot.computers c WHERE id=$1`, computerID).Scan(&record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	originalComputer := readComputer()
	execute(`INSERT INTO tombot.node_verifications(node_id,image_id,specification,runtime_id,runtime_generation,state,lease_expires_at)
		SELECT id,$2,'{}',runtime_id,generation,'checking',clock_timestamp()+interval '1 hour' FROM tombot.computer_nodes WHERE id=$1`, c.NodeID, oldImage)
	c.Registration.PreviousImageID = oldImage
	c.Registration.ImageID = ids.New()
	c.Registration.ImageReference = "registry.example.test/upgrade@sha256:" + strings.Repeat("b", 64)
	c.Maintenance = &MaintenanceRequest{OperationID: ids.New()}
	run := func(want string) {
		t.Helper()
		v, err := s.Register(ctx, signed(t, p, c), "", ids.New())
		if err != nil || v.State != want {
			t.Fatal(v, err)
		}
	}
	if _, err = s.Register(ctx, signed(t, p, c), "", ids.New()); !errors.Is(err, nodecatalog.ErrConflict) {
		t.Fatal("在途验收期间取得维护权", err)
	}
	execute(`UPDATE tombot.node_verifications SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE node_id=$1`, c.NodeID)
	run("upgrade_held")
	run("upgrade_held")
	if err := reserve(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("维护期间仍分配旧镜像电脑", err)
	}
	id := c.Maintenance.OperationID
	c.Maintenance.OperationID = ids.New()
	if _, err = s.Register(ctx, signed(t, p, c), "", ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("其他操作获取维护权", err)
	}
	c.Maintenance.OperationID = id
	c.Maintenance.Finish = true
	if _, err = s.Register(ctx, signed(t, p, c), "", ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("镜像尚未切换便结束维护", err)
	}
	maintenance := c.Maintenance
	c.Maintenance = nil
	run("registered")
	c.Maintenance = maintenance
	run("upgrade_finished")
	run("upgrade_finished")
	c.Maintenance.Finish = false
	if _, err = s.Register(ctx, signed(t, p, c), "", ids.New()); !errors.Is(err, ErrConflict) {
		t.Fatal("已完成的签名重新开启维护", err)
	}
	var reserved int
	if err = pool.QueryRowContext(ctx, `SELECT reserved FROM tombot.computer_nodes WHERE id=$1`, c.NodeID).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatal("升级改动已预留容量", reserved, err)
	}
	if readComputer() != originalComputer {
		t.Fatal("升级改动已有用户电脑记录")
	}
}
