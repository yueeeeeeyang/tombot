package dockernode

import (
	"context"
	"errors"
	"runtime"
	"time"

	"io.yueyang/tom/internal/computer"
)

// UpgradeComputer 关闭原运行实例并在原磁盘上启动新代次；中断后同一计划继续执行。
func (node *Runtime) UpgradeComputer(ctx context.Context, request computer.ImageUpgrade) (observation computer.Observation, result error) {
	if request.Validate() != nil || request.Target.NodeID != node.options.NodeID {
		return observation, failure("computer_image_upgrade_invalid")
	}
	remaining := time.Until(request.Target.LeaseExpiresAt)
	if remaining <= 0 || remaining > node.options.MaximumLease {
		return observation, failure("lease_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	release, err := node.journal.lock(ctx, request.Current.UserID)
	if err != nil {
		return observation, err
	}
	defer func() { result = errors.Join(result, release()) }()
	state, err := node.journal.read(request.Current.UserID)
	if err != nil {
		return observation, err
	}
	if state == nil || state.Retirement != nil {
		return observation, failure("computer_image_upgrade_source_missing")
	}
	if upgrade := state.ImageUpgrade; upgrade != nil && upgrade.Target.Generation == request.Target.Generation {
		if !sameImageUpgrade(*upgrade, request) || !validUpgradeFence(upgrade.Target, request.Target) ||
			state.Specification.Generation == request.Target.Generation && !validUpgradeFence(state.Specification, request.Target) {
			return observation, failure("computer_image_upgrade_stale")
		}
		state.ImageUpgrade = &request
		if state.Specification.Generation == request.Target.Generation {
			state.Specification = request.Target
		}
	} else {
		if err := node.prepareImageUpgrade(ctx, state, request); err != nil {
			return observation, err
		}
	}
	if state.Phase == "upgrading" {
		image, err := node.engine.inspectImage(ctx, request.Target.ImageReference)
		if err != nil {
			return observation, err
		}
		if err := node.verifyProgramImage(image, node.resource(request.Target)); err != nil {
			return observation, err
		}
		if err := image.validate(request.Target.ImageReference, "linux/"+runtime.GOARCH); err != nil {
			return observation, err
		}
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	if err := node.journal.save(state); err != nil {
		return observation, err
	}
	resource := node.withStorage(node.resource(state.Specification), state)
	if state.Phase == "upgrading" {
		if err := node.retireUpgradeSource(ctx, resource, state); err != nil {
			return observation, err
		}
		state.Specification = request.Target
		state.Phase = "creating"
		state.ContainerID = ""
		if err := node.journal.save(state); err != nil {
			return observation, err
		}
		resource = node.withStorage(node.resource(state.Specification), state)
	}
	if state.ContainerID != "" {
		current, err := node.load(ctx, resource, state)
		if err != nil {
			return observation, err
		}
		if !current.hasContainer {
			return observation, failure("computer_image_upgrade_target_missing")
		}
		if state.Phase == "ready" && current.container.State.Running {
			return node.inspect(ctx, resource, state)
		}
	}
	return node.ensure(ctx, resource, state)
}

func sameImageUpgrade(previous, request computer.ImageUpgrade) bool {
	return immutableEqual(previous.Current, request.Current) && immutableEqual(previous.Target, request.Target) && previous.CurrentContainerID == request.CurrentContainerID
}

func validUpgradeFence(previous, target computer.Specification) bool {
	return target.FencingToken > previous.FencingToken || target.FencingToken == previous.FencingToken && target.LeaseExpiresAt.Equal(previous.LeaseExpiresAt)
}

// UpgradeRecoveryProof 绑定控制服务已隔离的原电脑、目标镜像及最后一次切换的控制版本。
type UpgradeRecoveryProof struct {
	Source                              computer.Specification
	ContainerID, TargetImage, ErrorCode string
	TargetFence                         int64
}

// CheckSource 在申请维护窗口前核对本机记录。退役时仍会在用户锁内重新核验容器、挂载和原盘。
func (proof UpgradeRecoveryProof) CheckSource(ctx context.Context, directory string, owner uint32, socket string, timeout time.Duration) (result error) {
	source := proof.Source
	if source.Validate() != nil {
		return failure("computer_image_upgrade_source_mismatch")
	}
	journal, err := openJournal(directory, owner)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, journal.root.Close()) }()
	state, err := journal.read(source.UserID)
	if err != nil {
		return err
	}
	if state == nil || !immutableEqual(state.Specification, source) || state.ContainerID != proof.ContainerID {
		return failure("computer_image_upgrade_source_mismatch")
	}
	if proof.ErrorCode == "container_stop_unconfirmed" {
		plan := state.ImageUpgrade
		if state.Phase != "upgrading" || plan == nil || plan.Current.FencingToken != source.FencingToken || plan.Target.FencingToken != proof.TargetFence || plan.Target.ImageReference != proof.TargetImage {
			return failure("computer_image_upgrade_source_mismatch")
		}
		engine, err := newEngine(socket, timeout)
		if err != nil {
			return err
		}
		defer engine.close()
		current, err := engine.inspectContainer(ctx, proof.ContainerID)
		if err != nil {
			return err
		}
		if current.ID != proof.ContainerID || current.Name != "/"+resourceName(source) || current.Config.Image != source.ImageReference || current.State.Status != "exited" || current.State.Running || current.State.Pid != 0 || current.State.Dead || current.HostConfig.RestartPolicy.Name != "no" {
			return failure("container_stop_unconfirmed")
		}
		return journal.requireClosedSessions(ctx, source.UserID)
	}
	if state.Specification.FencingToken != source.FencingToken || (proof.ErrorCode != "" && proof.ErrorCode != "computer_image_upgrade_source_mismatch") {
		return failure("computer_image_upgrade_source_mismatch")
	}
	if proof.ErrorCode == "" {
		confirmed, err := journal.hasComputerRecovery(state)
		if err != nil {
			return err
		}
		if !confirmed {
			return failure("computer_image_upgrade_source_mismatch")
		}
	}
	return journal.requireUpgradeSource(state)
}

func (journal *journal) requireUpgradeSource(state *record) error {
	if state.Phase == "ready" {
		return nil
	}
	// 只有曾就绪的原容器会在启动复验前留下恢复标记；普通开户中的容器不能作为升级来源。
	recovering, err := journal.hasComputerRecovery(state)
	if err != nil {
		return err
	}
	if !recovering {
		return failure("computer_image_upgrade_source_mismatch")
	}
	return nil
}

func (node *Runtime) prepareImageUpgrade(ctx context.Context, state *record, request computer.ImageUpgrade) error {
	if !immutableEqual(state.Specification, request.Current) || state.ContainerID != request.CurrentContainerID || request.Target.FencingToken <= state.Specification.FencingToken {
		return failure("computer_image_upgrade_source_mismatch")
	}
	if err := node.journal.requireUpgradeSource(state); err != nil {
		return err
	}
	resource := node.withStorage(node.resource(state.Specification), state)
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return err
	}
	if !current.hasContainer || !current.workspace.Exists {
		return failure("computer_image_upgrade_source_missing")
	}
	if err := node.journal.requireClosedSessions(ctx, request.Current.UserID); err != nil {
		return err
	}
	if state.StorageGeneration == 0 {
		state.StorageGeneration = state.Specification.Generation
	}
	state.Phase = "upgrading"
	state.ImageUpgrade = &request
	return nil
}

func (node *Runtime) retireUpgradeSource(ctx context.Context, resource Resource, state *record) error {
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return err
	}
	if err := node.journal.requireClosedSessions(ctx, resource.Specification.UserID); err != nil {
		return err
	}
	if !current.workspace.Ready {
		workspace, err := node.storage.Restore(ctx, node.diskResource(resource, state))
		if err != nil {
			return err
		}
		if !workspace.Exists || !workspace.Ready || workspace.Path != resource.WorkspacePath || workspace.CapacityBytes != resource.Specification.Resources.DiskBytes {
			return failure("retained_storage_unconfirmed")
		}
	}
	// 升级意图已经落盘并接管旧实例，崩溃重试继续退役，不再尝试恢复旧容器。
	marker, err := node.journal.readFile(state.recoveryFile())
	if err != nil {
		return err
	}
	if marker != nil {
		if string(marker) != state.ImageUpgrade.CurrentContainerID {
			return failure("computer_image_upgrade_source_mismatch")
		}
		if err := node.journal.root.Remove(state.recoveryFile()); err != nil {
			return failure("computer_recovery_intent_remove_failed")
		}
	}
	return node.retireRuntime(ctx, resource, current, state.ImageUpgrade.CurrentContainerID)
}
