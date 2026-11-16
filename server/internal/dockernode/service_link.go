package dockernode

import (
	"context"
	"errors"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/processgroup"
)

// serviceParent 为控制检查、探针和浏览器使用同一实际 Docker 与内核身份规则，调用前后都须复验。
func (node *Runtime) serviceParent(ctx context.Context, resource Resource, id string, locate func(context.Context, string, int) (processgroup.ContainerReference, error)) (processgroup.ContainerReference, error) {
	current, err := node.serviceContainer(ctx, resource, id)
	if err != nil {
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

func (node *Runtime) serviceContainer(ctx context.Context, resource Resource, id string) (container, error) {
	current, err := node.engine.inspectContainer(ctx, id)
	if err != nil {
		return container{}, err
	}
	if current.ID != id || current.Name != "/"+resource.Name || !sameLabels(current.Config.Labels, containerLabels(resource)) || !current.State.Running || current.State.Dead || current.State.Pid <= 1 {
		return container{}, failure("service_container_mismatch")
	}
	if err := node.validateContainer(resource, current); err != nil {
		return container{}, err
	}
	return current, ctx.Err()
}

// serviceDeadline 只向当前已核验容器签发本地时钟期限；时钟换算前后都复验 PID 与资源组归属。
// 期限在写入授权记录前完成换算，后续查询、关闭及清理始终使用同一份不可变授权。
func (node *Runtime) serviceDeadline(ctx context.Context, resource Resource, expected processgroup.ContainerReference, locate func(context.Context, string, int) (processgroup.ContainerReference, error), deadline time.Time) (agentchannel.Deadline, error) {
	if expected.Validate() != nil || node.stampDeadline == nil {
		return agentchannel.Deadline{}, failure("service_clock_invalid")
	}
	current, err := node.serviceContainer(ctx, resource, expected.ContainerID)
	if err != nil {
		return agentchannel.Deadline{}, err
	}
	parent, err := locate(ctx, expected.ContainerID, current.State.Pid)
	if err != nil || parent != expected {
		return agentchannel.Deadline{}, errors.Join(failure("service_clock_parent_changed"), err)
	}
	stamp, err := node.stampDeadline(deadline, current.State.Pid)
	if err != nil {
		return agentchannel.Deadline{}, err
	}
	after, err := node.serviceContainer(ctx, resource, expected.ContainerID)
	if err != nil {
		return agentchannel.Deadline{}, err
	}
	parent, err = locate(ctx, expected.ContainerID, after.State.Pid)
	if err != nil || parent != expected || after.State.Pid != current.State.Pid {
		return agentchannel.Deadline{}, errors.Join(failure("service_clock_parent_changed"), err)
	}
	if err := ctx.Err(); err != nil {
		return agentchannel.Deadline{}, err
	}
	return stamp, nil
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
