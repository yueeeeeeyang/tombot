package nodeenrollment

import (
	"context"
	"errors"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestSignedIdleNodeImageUpgrade(t *testing.T) {
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
	for _, scenario := range []string{"unsigned_transition", "wrong_previous", "reserved", "success", "retry", "replay_old"} {
		t.Run(scenario, func(t *testing.T) {
			claim := upgrade
			switch scenario {
			case "unsigned_transition":
				claim.Registration.PreviousImageID = ""
			case "wrong_previous":
				claim.Registration.PreviousImageID = ids.New()
			case "reserved":
				if _, err := pool.Exec(`UPDATE tombot.computer_nodes SET reserved=1 WHERE id=$1`, initial.NodeID); err != nil {
					t.Fatal(err)
				}
			case "success":
				if _, err := pool.Exec(`UPDATE tombot.computer_nodes SET reserved=0 WHERE id=$1`, initial.NodeID); err != nil {
					t.Fatal(err)
				}
			case "replay_old":
				claim = initial
			}
			_, err := service.Register(t.Context(), signed(t, pair, claim), "", ids.New())
			if (err == nil) != (scenario == "success" || scenario == "retry") {
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
