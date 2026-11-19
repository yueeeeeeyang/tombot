package dockernode

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestImageUpgradeFromInterruptedRecovery(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	source, err := f.node.Ensure(ctx, f.specification)
	if err != nil {
		t.Fatal(err)
	}
	request := imageUpgradeRequest(f.specification, source.ContainerID, "b")
	resource := f.node.resource(f.specification)
	state, err := f.node.journal.read(f.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.node.load(ctx, resource, state)
	if err != nil {
		t.Fatal(err)
	}
	f.probe.coreMissing, f.probe.missing = true, true
	requireFailure(t, f.node.resumeComputer(ctx, resource, state, current), "computer_probe_failed")
	for _, marker := range []string{"", strings.Repeat("f", 64), source.ContainerID} {
		if marker == "" {
			err = f.node.journal.root.Remove(state.recoveryFile())
		} else {
			err = f.node.journal.writeFile(state.recoveryFile(), []byte(marker))
		}
		if err != nil {
			t.Fatal(err)
		}
		f.engine.events = nil
		_, err = f.node.UpgradeComputer(ctx, request)
		if marker != source.ContainerID {
			requireFailure(t, err, "computer_image_upgrade_source_mismatch")
			if len(f.engine.events) != 0 {
				t.Fatal("恢复标记缺失或不匹配时更改了原容器", f.engine.events)
			}
			continue
		}
		// 旧容器可以退役；新容器仍须通过独立的能力验收才能交给会话。
		requireFailure(t, err, "computer_probe_failed")
	}
	if _, err := f.node.journal.root.Stat(state.recoveryFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧代恢复标记未随已持久化的升级意图解除", err)
	}
	f.probe.coreMissing, f.probe.missing = false, false
	target, err := f.node.UpgradeComputer(ctx, request)
	if err != nil || !target.Matches(request.Target) || target.State != "ready" || !target.Capabilities.Ready() || target.ContainerID == source.ContainerID {
		t.Fatal("恢复中断后未完成原计划的目标验收", target, err)
	}
	if f.engine.creates != 2 || f.storage.ensures != 1 || f.storage.removals != 0 {
		t.Fatal("恢复升级时重复创建了容器或改变了原盘")
	}
}
