package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/processgroup"
)

// capabilityProbe 汇总本次真实检查、探针清理和宿主复验，任一失败都不提升电脑能力。
type capabilityProbe struct {
	node   *Runtime
	locate func(context.Context, string, int) (processgroup.ContainerReference, error)
}

func (probe *capabilityProbe) Check(ctx context.Context, resource Resource, containerID string, evidence computerprobe.Evidence) (computer.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return computer.Capabilities{}, err
	}
	state, err := probe.node.journal.readProbe(resource.Specification.UserID)
	if err != nil {
		return computer.Capabilities{}, err
	}
	if state == nil || state.Phase != "closed" || !immutableEqual(state.Specification, resource.Specification) || state.Specification.FencingToken != resource.Specification.FencingToken || !state.Specification.LeaseExpiresAt.Equal(resource.Specification.LeaseExpiresAt) || state.Grant.Request.Group.Container.ContainerID != containerID || evidence.Validate(state.Grant.Request.Group.ID) != nil {
		return computer.Capabilities{}, failure("capability_evidence_unconfirmed")
	}
	lifecycle, err := probe.node.journal.read(resource.Specification.UserID)
	if err != nil {
		return computer.Capabilities{}, err
	}
	if lifecycle == nil || !immutableEqual(lifecycle.Specification, resource.Specification) || lifecycle.Phase == "removing" || lifecycle.Phase == "absent" {
		return computer.Capabilities{}, failure("capability_lifecycle_changed")
	}
	current, err := probe.node.load(ctx, resource, lifecycle)
	if err != nil {
		return computer.Capabilities{}, err
	}
	if !current.hasContainer || current.container.ID != containerID || !current.container.State.Running || current.container.State.Dead || current.container.State.Pid <= 1 {
		return computer.Capabilities{}, failure("capability_container_changed")
	}
	parent, err := probe.locate(ctx, containerID, current.container.State.Pid)
	if err != nil {
		return computer.Capabilities{}, err
	}
	if parent != state.Grant.Request.Group.Container {
		return computer.Capabilities{}, failure("capability_parent_changed")
	}
	if err := probe.node.storage.VerifyFilesystem(ctx, probe.node.diskResource(resource, lifecycle), evidence.FilesystemType, evidence.FilesystemSize); err != nil {
		return computer.Capabilities{}, err
	}
	if err := probe.node.browsers.check(ctx, resource, current.container); err != nil {
		return computer.Capabilities{}, err
	}
	if err := ctx.Err(); err != nil {
		return computer.Capabilities{}, err
	}
	return computer.Capabilities{Terminal: evidence.Terminal, Browser: true, Files: evidence.Files, ProcessReaping: true, DiskQuota: true, NetworkIsolation: evidence.LocalNetwork}, nil
}
