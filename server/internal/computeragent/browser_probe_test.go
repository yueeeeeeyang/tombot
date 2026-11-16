package computeragent

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/browserprobe"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

type probeBrowser struct {
	unusedWindowBrowser
	call func(context.Context, string) (browserprobe.Evidence, error)
}

func (b probeBrowser) Probe(ctx context.Context, challenge string) (browserprobe.Evidence, error) {
	return b.call(ctx, challenge)
}

func TestBrowserControlProbeRequiresActiveIdentityAndCompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"ready", "inactive", "grant", "failed", "stale", "partial", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t)
			controller, _, factory := newBrowserControlFixture(t, fixture)
			grant := browserGrant(fixture)
			calls := 0
			factory.prepare = func(scope *fakeBrowserScope) {
				scope.start = func(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error) {
					return probeBrowser{call: func(ctx context.Context, challenge string) (browserprobe.Evidence, error) {
						calls++
						e := browserprobe.Evidence{ChallengeID: challenge, Sandbox: true, Windows: true, SharedStorage: true, Rendering: true, Input: true, Proxy: true, Cleanup: true}
						switch scenario {
						case "failed":
							return e, errors.New("测试探针失败")
						case "stale":
							e.ChallengeID = ids.New()
						case "partial":
							e.Sandbox = false
						}
						return e, ctx.Err()
					}}, nil
				}
			}
			if scenario != "inactive" {
				if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "grant" {
				grant.Revision++
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			challenge := ids.New()
			observed, err := controller.Probe(ctx, grant, challenge)
			if scenario == "ready" {
				if err != nil || observed.Evidence == nil || observed.Evidence.Validate(challenge) != nil || calls != 1 {
					t.Fatal(observed, err, calls)
				}
			} else if err == nil || observed.Evidence != nil {
				t.Fatal("失败探针返回了证据", observed, err)
			}
		})
	}
}
