package dockernode

import (
	"context"
	"slices"
	"strings"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type maintenanceEngine struct{ *recoveryTestEngine }

func (e maintenanceEngine) stopContainer(ctx context.Context, id string) error {
	if err := e.fakeEngine.stopContainer(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current, _ := e.findContainer(id)
	current.State.Status = "exited"
	e.containers[strings.TrimPrefix(current.Name, "/")] = current
	return nil
}

func TestUserRestartsOriginalComputer(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		f := newFixture(t)
		s := f.prepareComputer(t, f.specification)
		c := f.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
		f.probe.missing = true // 浏览器尚未修复也必须能重启原电脑。
		f.node.engine = maintenanceEngine{&recoveryTestEngine{fakeEngine: f.engine}}
		res := f.node.resource(f.specification)
		if stopped {
			simulateComputerShutdown(t, f, res, res)
		} else if _, err := c.Open(t.Context(), commandFor(s)); err != nil {
			t.Fatal("无法开启重启前的活动桌面", err)
		}
		f.engine.events = nil
		m := computer.Maintenance{UserID: s.UserID, ComputerID: s.ComputerID, NodeID: s.NodeID, ContainerID: s.ContainerID, Generation: s.Generation}
		wrong := m
		wrong.ComputerID = ids.New()
		if _, err := c.ManageComputer(t.Context(), wrong); err == nil {
			t.Fatal("接受了错误电脑身份")
		}
		status, err := c.ManageComputer(t.Context(), m)
		if err != nil || !status.Matches(m) || (status.State == "stopped") != stopped {
			t.Fatal(status, err)
		}
		request := m
		request.ExpectedVersion = status.Version
		accepted, err := c.ManageComputer(t.Context(), request)
		if err != nil || accepted.State != "restarting" {
			t.Fatal(accepted, err)
		}
		c.pending.Wait()
		if !stopped {
			previous, err := f.node.journal.readSession(sessionKeyOf(s))
			if err != nil || previous == nil || previous.Phase != "closed" {
				t.Fatal("重启未封闭原会话", previous, err)
			}
			if _, exists := c.lease(sessionKeyOf(s)); exists {
				t.Fatal("重启后原桌面仍持有执行租约")
			}
		}
		finished, err := c.ManageComputer(t.Context(), m)
		if err != nil || finished.State != "running" || finished.Version != accepted.Version {
			t.Fatal(finished, err)
		}
		replay, err := c.ManageComputer(t.Context(), request)
		if err != nil || replay.State != "running" {
			t.Fatal(replay, err)
		}
		want := []string{"stop-container", "start-container"}
		if stopped {
			want = want[1:]
		}
		if !slices.Equal(f.engine.events, want) || f.storage.removals != 0 {
			t.Fatal("重复重启或删除了磁盘", f.engine.events)
		}
	}
}
