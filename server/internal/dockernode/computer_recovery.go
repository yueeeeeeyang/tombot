package dockernode

import (
	"context"
	"errors"
	"log/slog"

	"io.yueyang/tom/internal/computer"
)

func exitedComputer(current container) bool {
	return current.State.Status == "exited" && !current.State.Running && !current.State.Dead && current.State.Pid == 0
}

// hasComputerRecovery 以原容器 ID 区分重启中断与尚未完成的开户或镜像升级。
// 就绪记录可能在同步目录失败后已经可见，恢复意图须在就绪记录写入完成后才解除。
func (journal *journal) hasComputerRecovery(state *record) (bool, error) {
	if state.ContainerID == "" || state.Phase != "creating" && state.Phase != "ready" {
		return false, nil
	}
	containerID, err := journal.readFile(state.recoveryFile())
	return string(containerID) == state.ContainerID, err
}

func (state *record) recoveryFile() string { return ".recover-" + state.Specification.UserID }

// restoreComputer 在用户独占锁内恢复原实例。单台电脑恢复失败时先确认出网已封闭，再继续下一台。
func (node *Runtime) restoreComputer(ctx context.Context, logger *slog.Logger, resource Resource, state *record, current snapshot) error {
	budget := node.options.CallTimeout + node.options.AgentStartup.Timeout + node.options.Browser.StartupTimeout + node.options.Browser.CallTimeout + node.options.Probes.CallTimeout
	attempt, cancel := context.WithTimeout(ctx, budget)
	err := node.resumeComputer(attempt, resource, state, current)
	cancel()
	if err != nil {
		logComputerRecovery(ctx, logger, state, err)
		cleanup, stop := context.WithTimeout(ctx, node.options.Egress.ShutdownTimeout)
		defer stop()
		if closeErr := node.egress.stop(cleanup, resource); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return ctx.Err()
	}
	logger.InfoContext(ctx, "原电脑容器恢复就绪", "node_id", resource.Specification.NodeID, "user_id", resource.Specification.UserID, "computer_id", resource.Specification.ComputerID, "container_id", state.ContainerID)
	return nil
}

func (node *Runtime) resumeComputer(ctx context.Context, resource Resource, state *record, current snapshot) error {
	if !current.hasContainer || current.container.State.Dead || (!current.container.State.Running && !exitedComputer(current.container)) {
		return failure("computer_recovery_container_unavailable")
	}
	if state.Phase == "ready" {
		// 先保存原容器身份，再关闭会话准入。启动回包丢失时也必须重新验证实际能力。
		if err := node.journal.writeFile(state.recoveryFile(), []byte(state.ContainerID)); err != nil {
			return err
		}
		state.Phase = "creating"
		if err := node.journal.save(state); err != nil {
			return err
		}
	}
	if !current.container.State.Running {
		if err := node.agent.prepareRestart(ctx, resource, state); err != nil {
			return err
		}
		if !current.workspace.Ready {
			workspace, err := node.storage.Restore(ctx, node.diskResource(resource, state))
			if err != nil {
				return err
			}
			if !workspace.Exists || !workspace.Ready || workspace.Path != resource.WorkspacePath || workspace.CapacityBytes != resource.Specification.Resources.DiskBytes {
				return failure("workspace_not_ready")
			}
		}
		if err := node.engine.startContainer(ctx, state.ContainerID); err != nil {
			return err
		}
	}
	observation, err := node.inspect(ctx, resource, state)
	if err != nil {
		return err
	}
	if observation.State != "ready" {
		return failure("computer_probe_failed")
	}
	return nil
}

func logComputerRecovery(ctx context.Context, logger *slog.Logger, state *record, err error) {
	code := "computer_recovery_failed"
	var confirmed *computer.Failure
	if errors.As(err, &confirmed) {
		code = confirmed.Code
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "computer_recovery_timeout"
	}
	specification := state.Specification
	logger.WarnContext(ctx, "电脑容器恢复失败，暂不开放新会话", "node_id", specification.NodeID, "user_id", specification.UserID, "computer_id", specification.ComputerID, "container_id", state.ContainerID, "error_code", code)
}
