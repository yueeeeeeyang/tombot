package dockernode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"io.yueyang/tom/internal/noderelease"
)

type programTestEngine struct{ *recoveryTestEngine }

func TestVerificationRetiresProgramSelection(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "上一版程序"
		if damaged {
			name = "损坏的程序选择记录"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.node.engine = programTestEngine{&recoveryTestEngine{fakeEngine: f.engine}}
			f.node.options.ProgramCore = strings.Repeat("a", 64)
			f.node.options.ProgramImage = f.specification.ImageReference
			if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
				t.Fatal(err)
			}
			if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
				t.Fatal(err)
			}
			dir := f.node.resource(f.specification).ControlPath
			revision := filepath.Join(dir, "program-core")
			if damaged {
				if err := os.WriteFile(revision, []byte("invalid\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.node.options.ProgramCore = strings.Repeat("b", 64)
			next := advanceSpecification(f.specification)
			_, err := f.node.Inspect(t.Context(), next)
			if damaged {
				requireFailure(t, err, "program_revision_unconfirmed")
				if _, err := os.Stat(revision); err != nil {
					t.Fatal("损坏的选择记录应保留", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("上一代验收电脑的控制目录未回收", err)
			}
			if observation, err := f.node.Ensure(t.Context(), next); err != nil || observation.State != "ready" {
				t.Fatal("下一代验收电脑未就绪", observation.State, err)
			}
		})
	}
}

func (e programTestEngine) inspectImage(ctx context.Context, ref string) (imageDescription, error) {
	image, err := e.fakeEngine.inspectImage(ctx, ref)
	image.Config.Labels[noderelease.ProgramImageLabel] = "1"
	return image, err
}

func (e programTestEngine) stopContainer(ctx context.Context, id string) error {
	if err := e.fakeEngine.stopContainer(ctx, id); err != nil {
		return err
	}
	c, _ := e.findContainer(id)
	c.State.Status = "exited"
	e.containers[strings.TrimPrefix(c.Name, "/")] = c
	return nil
}

func TestMountedCoreChangesRestartOnlyTheOriginalComputer(t *testing.T) {
	f := newFixture(t)
	f.node.engine = programTestEngine{&recoveryTestEngine{fakeEngine: f.engine}}
	f.node.options.ProgramCore = strings.Repeat("a", 64)
	f.node.options.ProgramImage = f.specification.ImageReference
	original, err := f.node.Ensure(t.Context(), f.specification)
	if err != nil {
		t.Fatal(err)
	}
	res := f.node.resource(f.specification)
	c := f.engine.containers[res.Name]
	if m := c.HostConfig.Mounts[len(c.HostConfig.Mounts)-1]; m.Target != noderelease.ProgramRoot || !m.ReadOnly {
		t.Fatal(m)
	}
	drift := c
	drift.Mounts = slices.Clone(c.Mounts)
	drift.Mounts[len(drift.Mounts)-1].RW = true
	if f.node.validateContainer(res, drift) == nil {
		t.Fatal("可写程序挂载未被拒绝")
	}
	f.engine.events = nil
	for _, rev := range []string{"a", "b", "b", "a"} {
		f.node.options.ProgramCore = strings.Repeat(rev, 64)
		if err := f.node.restoreUserEgress(t.Context(), sessionLogger(), f.specification.UserID); err != nil {
			t.Fatal(err)
		}
		actual, err := f.node.selectedComputerPrograms(res)
		if err != nil || actual != f.node.options.ProgramCore {
			t.Fatal(actual, err)
		}
		c = f.engine.containers[res.Name]
		if c.ID != original.ContainerID || !c.State.Running || f.storage.removals != 0 {
			t.Fatal("切换程序改变电脑或磁盘")
		}
	}
	if !slices.Equal(f.engine.events, []string{"stop-container", "start-container", "stop-container", "start-container"}) {
		t.Fatal(f.engine.events)
	}
}
