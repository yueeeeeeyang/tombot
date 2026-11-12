package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

// ProbeOptions 分别约束实际检查和独立回收；回收预算不能被已过期的业务请求消耗。
type ProbeOptions struct {
	CallTimeout    time.Duration
	CleanupTimeout time.Duration
	Groups         processgroup.Options
}

// Validate 检查显式时限及资源组参数；缺失配置或不足以回收的预算均返回错误。
func (options ProbeOptions) Validate() error {
	if err := options.Groups.Validate(); err != nil {
		return err
	}
	if options.CallTimeout < time.Second || options.CallTimeout > taskprocess.ProbeMaximumLifetime || options.CleanupTimeout < time.Second || options.CleanupTimeout > time.Minute || options.CleanupTimeout <= options.Groups.GracePeriod+options.Groups.PollInterval {
		return errors.New("节点基础探针时限无效")
	}
	return nil
}

type computerProbes interface {
	check(context.Context, Resource, container) (computerprobe.Evidence, error)
	retire(context.Context, Resource) error
}

// nodeProbes 的运行操作持有用户生命周期独占锁；恢复自行逐用户加锁，不能递归调用运行操作。
type nodeProbes struct {
	node    *Runtime
	store   probeStore
	groups  processgroup.ServiceGroups
	options ProbeOptions
	logger  *slog.Logger
	locate  func(context.Context, string, int) (processgroup.ContainerReference, error)
	stamp   func(time.Time) (agentchannel.Deadline, error)
	invoke  func(context.Context, Resource, computeragent.ProbeGrant, []*os.File) (computerprobe.Evidence, error)
}

func newProbes(node *Runtime, logger *slog.Logger) (*nodeProbes, error) {
	groups, err := processgroup.NewServices(node.options.Probes.Groups, logger)
	if err != nil {
		return nil, err
	}
	probes := &nodeProbes{node: node, store: node.journal, groups: groups, options: node.options.Probes, logger: logger, locate: processgroup.LocateContainer, stamp: agentchannel.FromDeadline}
	probes.invoke = probes.call
	return probes, nil
}

func (probes *nodeProbes) previous(resource Resource) (*probeRecord, error) {
	state, err := probes.store.readProbe(resource.Specification.UserID)
	if err != nil || state == nil {
		return state, err
	}
	previous, current := state.Specification, resource.Specification
	if err := state.validate(current.UserID); err != nil {
		return nil, err
	}
	if previous.ComputerID != current.ComputerID || previous.NodeID != current.NodeID || previous.OperationID != current.OperationID || previous.VolumeReference != current.VolumeReference || previous.Generation > current.Generation || previous.Generation == current.Generation && !immutableEqual(previous, current) {
		return nil, failure("probe_instance_mismatch")
	}
	return state, nil
}

func (probes *nodeProbes) check(ctx context.Context, resource Resource, current container) (evidence computerprobe.Evidence, result error) {
	ctx, cancel := context.WithTimeout(ctx, probes.options.CallTimeout)
	defer cancel()
	previous, err := probes.previous(resource)
	if err != nil {
		return evidence, err
	}
	revision := int64(1)
	if previous != nil {
		if previous.Phase != "closed" {
			if err := probes.close(previous); err != nil {
				return evidence, err
			}
		}
		if previous.Grant.Revision == math.MaxInt64 {
			return evidence, failure("probe_revision_exhausted")
		}
		revision = previous.Grant.Revision + 1
	}
	parent, err := probes.parent(ctx, resource, current.ID)
	if err != nil {
		return evidence, err
	}
	deadline, _ := ctx.Deadline()
	stamp, err := probes.stamp(deadline)
	if err != nil {
		return evidence, err
	}
	specification := resource.Specification
	grant := computeragent.ProbeGrant{Identity: computeragent.Identity{UserID: specification.UserID, ComputerID: specification.ComputerID, NodeID: specification.NodeID, Generation: specification.Generation}, Revision: revision, Request: taskprocess.ProbeRequest{Version: 1, Group: processgroup.ServiceReference{Container: parent, ID: ids.New(), Kind: processgroup.ProbeService}, Deadline: stamp}}
	state := &probeRecord{Version: 1, Specification: specification, Grant: grant, Phase: "prepared"}
	if err := probes.store.saveProbe(state); err != nil {
		return evidence, err
	}
	// Create 的结果也可能丢失；从持久意图开始，任何失败都必须核对并封闭同一个服务组。
	defer func() {
		result = errors.Join(result, probes.close(state))
		if result == nil {
			observed, err := probes.parent(ctx, resource, current.ID)
			result = err
			if err == nil && observed != parent {
				result = failure("probe_parent_changed")
			}
		}
		if result != nil {
			evidence = computerprobe.Evidence{}
		}
	}()
	if err := probes.groups.Create(ctx, grant.Request.Group); err != nil {
		return evidence, err
	}
	files, err := probes.groups.Controls(ctx, grant.Request.Group)
	if err != nil {
		return evidence, err
	}
	defer func() { result = errors.Join(result, agentchannel.CloseFiles(files)) }()
	state.Phase = "dispatched"
	if err := probes.store.saveProbe(state); err != nil {
		return evidence, err
	}
	if err := ctx.Err(); err != nil {
		return evidence, err
	}
	evidence, err = probes.invoke(ctx, resource, grant, files)
	if err != nil {
		return computerprobe.Evidence{}, err
	}
	if err := evidence.Validate(grant.Request.Group.ID); err != nil {
		return computerprobe.Evidence{}, err
	}
	return evidence, ctx.Err()
}

// close 保留未知执行屏障；只有内核回收及最终记录均成功才允许后续检查使用新版本。
func (probes *nodeProbes) close(state *probeRecord) (result error) {
	if state.Phase == "closed" {
		return nil
	}
	defer func() {
		if result != nil {
			probes.logger.Error("电脑基础探针回收未确认", "user_id", state.Specification.UserID, "computer_id", state.Specification.ComputerID, "generation", state.Specification.Generation, "service_id", state.Grant.Request.Group.ID, "reason", result.Error())
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), probes.options.CleanupTimeout)
	defer cancel()
	state.Phase = "closing"
	saveErr := probes.store.saveProbe(state)
	drainErr := probes.groups.Drain(ctx, state.Grant.Request.Group)
	if err := errors.Join(saveErr, drainErr, ctx.Err()); err != nil {
		return err
	}
	state.Phase = "closed"
	return probes.store.saveProbe(state)
}

func (probes *nodeProbes) retire(ctx context.Context, resource Resource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := probes.previous(resource)
	if err != nil || state == nil {
		return err
	}
	return probes.close(state)
}

// recover 在节点开放 API 和恢复出网前，只封闭已记录实例，不重放探针请求。
func (probes *nodeProbes) recover(ctx context.Context) error {
	return probes.store.visitProbes(ctx, func(userID string) (result error) {
		release, err := probes.node.journal.lock(ctx, userID)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, release()) }()
		state, err := probes.node.journal.read(userID)
		if err != nil {
			return err
		}
		if state == nil || state.Specification.NodeID != probes.node.options.NodeID {
			return failure("probe_lifecycle_missing")
		}
		return probes.retire(ctx, probes.node.resource(state.Specification))
	})
}

// parent 在调用前、校验对端时及完成回收后复验实际 Docker 和内核归属。
func (probes *nodeProbes) parent(ctx context.Context, resource Resource, id string) (processgroup.ContainerReference, error) {
	return probes.node.serviceParent(ctx, resource, id, probes.locate)
}

func (probes *nodeProbes) call(ctx context.Context, resource Resource, grant computeragent.ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
	client, err := probes.node.serviceClient(ctx, resource, grant.Request.Group.Container, probes.locate, probes.options.CallTimeout)
	if err != nil {
		return computerprobe.Evidence{}, err
	}
	return client.Probe(ctx, grant, files)
}
