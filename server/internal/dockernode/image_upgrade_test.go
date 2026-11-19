package dockernode

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func imageUpgradeRequest(current computer.Specification, containerID, digestDigit string) computer.ImageUpgrade {
	target := current
	target.ImageReference = "registry.example.test/computer@sha256:" + strings.Repeat(digestDigit, 64)
	target.Generation++
	target.Attempt++
	target.FencingToken++
	target.LeaseExpiresAt = time.Now().Add(time.Minute)
	return computer.ImageUpgrade{Current: current, Target: target, CurrentContainerID: containerID}
}

func TestImageUpgradeKeepsDiskAcrossGenerations(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	computer, err := f.node.Ensure(ctx, f.specification)
	if err != nil {
		t.Fatal(err)
	}
	original := f.node.resource(f.specification)
	if err := f.engine.stopContainer(ctx, computer.ContainerID); err != nil {
		t.Fatal(err)
	}
	workspace := f.storage.workspaces[original.Name]
	workspace.Ready = false
	f.storage.workspaces[original.Name] = workspace
	f.engine.events = nil

	request := imageUpgradeRequest(f.specification, computer.ContainerID, "b")
	for _, image := range []string{"b", "c"} {
		if image == "c" {
			request = imageUpgradeRequest(request.Target, computer.ContainerID, image)
		}
		computer, err = f.node.UpgradeComputer(ctx, request)
		if err != nil || !computer.Matches(request.Target) || computer.State != "ready" || !computer.Capabilities.Ready() || computer.ContainerID == request.CurrentContainerID {
			t.Fatalf("新实例未通过验收：%+v，%v", computer, err)
		}
		resource := f.node.resource(request.Target)
		volume, err := f.engine.inspectVolume(ctx, resource.Name)
		if err != nil || volume.Options["device"] != original.WorkspacePath {
			t.Fatalf("新实例没有挂载原磁盘：%+v，%v", volume, err)
		}
		if len(f.engine.containers) != 1 || len(f.engine.volumes) != 1 || len(f.storage.workspaces) != 1 {
			t.Fatal("同一电脑存在多份运行资源或持久磁盘")
		}
		state, err := f.node.journal.read(request.Target.UserID)
		if err != nil || state.StorageGeneration != original.Specification.Generation {
			t.Fatalf("持久磁盘代次变化：%+v，%v", state, err)
		}
		if f.node.withStorage(resource, state).ControlPath == original.ControlPath {
			t.Fatal("新实例复用了已关闭的控制目录")
		}
	}
	transition := []string{"stop-container", "remove-container", "remove-volume", "create-volume", "create-container", "start-container"}
	if !slices.Equal(f.engine.events, append(slices.Clone(transition), transition...)) {
		t.Fatalf("旧写入实例未在新代建立前退出：%v", f.engine.events)
	}
	if f.storage.ensures != 1 || f.storage.restores != 1 || f.storage.removals != 0 {
		t.Fatal("更新创建或删除了持久磁盘，或未恢复重启后丢失的挂载")
	}

	_, err = f.node.Remove(ctx, request.Target)
	requireFailure(t, err, "retained_storage_removal_forbidden")
	_, err = f.node.Ensure(ctx, request.Target)
	requireFailure(t, err, "retained_storage_requires_upgrade")
	_, err = f.node.Remove(ctx, f.specification)
	requireFailure(t, err, "generation_not_released")
	if current, err := f.node.Inspect(ctx, request.Target); err != nil || current.ContainerID != computer.ContainerID || current.State != "ready" {
		t.Fatalf("常规对账没有验证保留的磁盘：%+v，%v", current, err)
	}
}

func TestImageUpgradeResumesOldRetirementAndUnknownCreation(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	original, err := f.node.Ensure(ctx, f.specification)
	if err != nil {
		t.Fatal(err)
	}
	request := imageUpgradeRequest(f.specification, original.ContainerID, "b")
	f.engine.removeVolumeError = context.DeadlineExceeded
	if _, err := f.node.UpgradeComputer(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("原容器退役中断未返回", err)
	}
	state, err := f.node.journal.read(request.Current.UserID)
	if err != nil || state.Phase != "upgrading" || state.ContainerID != original.ContainerID {
		t.Fatalf("换代意图没有在旧实例变更前持久化：%+v，%v", state, err)
	}
	if _, err := f.engine.inspectContainer(ctx, original.ContainerID); !errors.Is(err, errMissing) || f.engine.creates != 1 {
		t.Fatal("旧容器未退出，或卷回收不明时创建了新容器", err)
	}
	_, err = f.node.Ensure(ctx, request.Current)
	requireFailure(t, err, "computer_image_upgrade_in_progress")
	_, err = f.node.Remove(ctx, request.Current)
	requireFailure(t, err, "computer_image_upgrade_in_progress")

	journal, err := openJournal(f.statePath, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.root.Close(); err != nil {
			t.Error(err)
		}
	})
	restarted := *f.node
	restarted.journal = journal
	if err := restarted.restoreUserEgress(ctx, sessionLogger(), request.Current.UserID); err != nil {
		t.Fatal("节点重启未跳过正在退役的实例", err)
	}
	f.engine.removeVolumeError = nil
	f.engine.lostCreate = true
	if _, err := restarted.UpgradeComputer(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("新容器创建的未知回包未保留", err)
	}
	oldRequest := request
	request.Target.FencingToken++
	request.Target.LeaseExpiresAt = time.Now().Add(time.Minute)
	f.engine.lostCreate = false
	f.probe.missing = true
	_, err = restarted.UpgradeComputer(ctx, request)
	requireFailure(t, err, "computer_probe_failed")
	if f.engine.creates != 2 {
		t.Fatal("创建回包未知后又创建了容器")
	}
	_, err = restarted.UpgradeComputer(ctx, oldRequest)
	requireFailure(t, err, "computer_image_upgrade_stale")
	f.probe.missing = false
	current, err := restarted.UpgradeComputer(ctx, request)
	if err != nil || current.State != "ready" || !current.Capabilities.Ready() {
		t.Fatalf("同一个计划无法在能力就绪后收尾：%+v，%v", current, err)
	}
	retried, err := restarted.UpgradeComputer(ctx, request)
	if err != nil || retried != current || f.engine.creates != 2 || f.storage.ensures != 1 || f.storage.removals != 0 {
		t.Fatalf("重复领取没有使用原有的完成结果：%+v，%v", retried, err)
	}
}

func TestImageUpgradeRefusesUnconfirmedSource(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(f fixture, upgrade *computer.ImageUpgrade)
		code   string
	}{
		{"原容器身份不符", func(f fixture, upgrade *computer.ImageUpgrade) { upgrade.CurrentContainerID = strings.Repeat("f", 64) }, "computer_image_upgrade_source_mismatch"},
		{"升级授权已到期", func(f fixture, upgrade *computer.ImageUpgrade) {
			upgrade.Target.LeaseExpiresAt = time.Now().Add(-time.Second)
		}, "lease_invalid"},
		{"目标镜像无法读取", func(f fixture, _ *computer.ImageUpgrade) { f.engine.imageError = failure("target_image_missing") }, "target_image_missing"},
		{"原磁盘丢失", func(f fixture, _ *computer.ImageUpgrade) {
			delete(f.storage.workspaces, f.node.resource(f.specification).Name)
		}, "container_dependencies_missing"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			current, err := f.node.Ensure(t.Context(), f.specification)
			if err != nil {
				t.Fatal(err)
			}
			request := imageUpgradeRequest(f.specification, current.ContainerID, "b")
			scenario.change(f, &request)
			f.engine.events = nil
			_, err = f.node.UpgradeComputer(t.Context(), request)
			requireFailure(t, err, scenario.code)
			if len(f.engine.events) != 0 || f.storage.removals != 0 {
				t.Fatalf("原实例未核实前发生资源变更：%v", f.engine.events)
			}
		})
	}
}

func TestImageUpgradeWaitsForAllSessionClosures(t *testing.T) {
	f := newFixture(t)
	session := f.prepareComputer(t, f.specification)
	request := imageUpgradeRequest(f.specification, session.ContainerID, "b")
	state := &sessionRecord{Version: 2, Command: commandFor(session), Epoch: ids.New(), Phase: "closing", Deadline: time.Now().UTC()}
	if err := f.node.journal.saveSession(state); err != nil {
		t.Fatal(err)
	}
	f.engine.events = nil
	_, err := f.node.UpgradeComputer(t.Context(), request)
	requireFailure(t, err, "computer_session_active")
	if len(f.engine.events) != 0 {
		t.Fatalf("旧会话关闭不明时修改了容器：%v", f.engine.events)
	}
	state.Phase = "closed"
	if err = f.node.journal.saveSession(state); err != nil {
		t.Fatal(err)
	}
	if observation, err := f.node.UpgradeComputer(t.Context(), request); err != nil || observation.State != "ready" {
		t.Fatalf("全部会话关闭后无法升级：%+v，%v", observation, err)
	}
}
