package dockernode

import (
	"context"
	"errors"
	"io.yueyang/tom/internal/browserprobe"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"testing"
)

func TestBrowserProbeBindsLiveGrantAndFreshEvidence(t *testing.T) {
	for _, scenario := range []string{"ready", "failed", "missing", "partial", "stale", "grant", "closed", "parent", "cancelled", "inactive"} {
		t.Run(scenario, func(t *testing.T) {
			_, b, _, resource, current, _ := newBrowserFixture(t)
			if scenario != "inactive" {
				if err := b.ensure(t.Context(), resource, current); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			b.probeInvoke = func(_ context.Context, _ Resource, grant computeragent.BrowserGrant, challenge string) (computeragent.BrowserObservation, error) {
				e := browserprobe.Evidence{ChallengeID: challenge, Sandbox: true, Windows: true, SharedStorage: true, Rendering: true, Input: true, Proxy: true, Cleanup: true}
				observed := computeragent.BrowserObservation{Grant: grant, Phase: "active", Evidence: &e}
				switch scenario {
				case "failed":
					return observed, errors.New("测试探针失败")
				case "missing":
					observed.Evidence = nil
				case "partial":
					e.Cleanup = false
				case "stale":
					e.ChallengeID = ids.New()
				case "grant":
					observed.Grant.Revision++
				case "closed":
					observed.Phase = "closed"
				case "parent":
					original := b.locate
					b.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
						parent, err := original(ctx, id, pid)
						parent.Inode++
						return parent, err
					}
				}
				return observed, nil
			}
			if err := b.check(ctx, resource, current); (err == nil) != (scenario == "ready") {
				t.Fatal(scenario, err)
			}
		})
	}
}
