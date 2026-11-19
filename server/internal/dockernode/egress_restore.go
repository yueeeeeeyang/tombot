package dockernode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/oklog/ulid/v2"
)

// restoreEgress 在开放节点前逐批恢复原电脑和出网端点。调用方已持有节点服务锁并关闭旧执行。
// 单台电脑恢复失败时保留准入屏障；未知生命周期或出网无法封闭时拒绝节点启动。
func (node *Runtime) restoreEgress(ctx context.Context, logger *slog.Logger) (result error) {
	directory, err := node.journal.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == runtimeRecordName || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".session.json") || strings.HasSuffix(name, ".probe.json") || strings.HasSuffix(name, ".browser.json") {
				continue
			}
			userID := strings.TrimSuffix(name, ".json")
			if _, err := ulid.ParseStrict(userID); err != nil {
				return failure("egress_journal_name_invalid")
			}
			if err := node.restoreUserEgress(ctx, logger, userID); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func (node *Runtime) restoreUserEgress(ctx context.Context, logger *slog.Logger, userID string) (result error) {
	release, err := node.journal.lock(ctx, userID)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	state, err := node.journal.read(userID)
	if err != nil {
		return err
	}
	if state == nil || state.Specification.NodeID != node.options.NodeID {
		return failure("egress_journal_identity_invalid")
	}
	resource := node.withStorage(node.resource(state.Specification), state)
	if state.restarting() {
		attempt, cancel := context.WithTimeout(ctx, node.restartBudget())
		defer cancel()
		if err := node.finishRestart(attempt, logger, state, func(ctx context.Context) error { return node.journal.requireClosedSessions(ctx, userID) }); err != nil {
			logComputerRecovery(ctx, logger, state, err)
			return node.egress.stop(ctx, resource)
		}
		return nil
	}
	if state.RestartPhase == "failed" {
		return node.egress.stop(ctx, resource)
	}
	resuming, err := node.journal.hasComputerRecovery(state)
	if err != nil {
		return err
	}
	if state.upgradeInProgress() && !resuming {
		return node.egress.stop(ctx, resource)
	}
	current, err := node.load(ctx, resource, state)
	wasReady := state.Phase == "ready" && state.ContainerID != ""
	if err != nil {
		if resuming || wasReady && current.hasContainer && !current.container.State.Running {
			logComputerRecovery(ctx, logger, state, err)
			return node.egress.stop(ctx, resource)
		}
		return err
	}
	if state.Phase == "absent" && !current.absent() {
		return failure("closed_generation_has_resources")
	}
	if wasReady && current.hasContainer && current.container.State.Running && resource.ExternalPrograms {
		core, err := node.selectedComputerPrograms(resource)
		if err != nil {
			return err
		}
		if core != node.options.ProgramCore {
			return node.restoreComputer(ctx, logger, resource, state, current)
		}
	}
	if resuming || wasReady && !current.container.State.Running {
		return node.restoreComputer(ctx, logger, resource, state, current)
	}
	if current.hasContainer && current.container.State.Running && state.Phase != "removing" && state.Phase != "absent" {
		return node.egress.ensure(ctx, resource, current.container)
	}
	return node.egress.stop(ctx, resource)
}
