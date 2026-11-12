package dockernode

import (
	"context"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// serviceParent 为控制检查、探针和浏览器使用同一实际 Docker 与内核身份规则，调用前后都须复验。
func (node *Runtime) serviceParent(ctx context.Context, resource Resource, id string, locate func(context.Context, string, int) (processgroup.ContainerReference, error)) (processgroup.ContainerReference, error) {
	current, err := node.engine.inspectContainer(ctx, id)
	if err != nil {
		return processgroup.ContainerReference{}, err
	}
	if current.ID != id || current.Name != "/"+resource.Name || !sameLabels(current.Config.Labels, resource.Labels) || !current.State.Running || current.State.Dead || current.State.Pid <= 1 {
		return processgroup.ContainerReference{}, failure("service_container_mismatch")
	}
	if err := node.validateContainer(resource, current); err != nil {
		return processgroup.ContainerReference{}, err
	}
	reference, err := locate(ctx, id, current.State.Pid)
	if err != nil {
		return processgroup.ContainerReference{}, err
	}
	if reference.Validate() != nil || reference.ContainerID != id {
		return processgroup.ContainerReference{}, failure("service_parent_mismatch")
	}
	return reference, ctx.Err()
}

// serviceClient 只接受原容器内的 root 代理；入口路径、对端 PID 和真实父组均不由请求覆盖。
func (node *Runtime) serviceClient(ctx context.Context, resource Resource, expected processgroup.ContainerReference, locate func(context.Context, string, int) (processgroup.ContainerReference, error), timeout time.Duration) (*computeragent.Client, error) {
	return node.agentClient(resource, func(peer agentchannel.Peer) error {
		if peer.PID <= 1 || peer.UID != 0 || peer.GID != 0 {
			return failure("service_peer_invalid")
		}
		actual, err := node.serviceParent(ctx, resource, expected.ContainerID, locate)
		if err != nil {
			return err
		}
		observed, err := locate(ctx, expected.ContainerID, int(peer.PID))
		if err != nil {
			return err
		}
		if actual != expected || observed != expected {
			return failure("service_peer_container_mismatch")
		}
		return nil
	}, timeout)
}
