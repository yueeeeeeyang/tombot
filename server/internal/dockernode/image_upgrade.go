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

func (node *Runtime) prepareImageUpgrade(ctx context.Context, state *record, request computer.ImageUpgrade) error {
	if state.Phase != "ready" || !immutableEqual(state.Specification, request.Current) || state.ContainerID != request.CurrentContainerID || request.Target.FencingToken <= state.Specification.FencingToken {
		return failure("computer_image_upgrade_source_mismatch")
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
	return node.retireRuntime(ctx, resource, current, state.ImageUpgrade.CurrentContainerID)
}
