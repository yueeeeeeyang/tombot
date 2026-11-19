package dockernode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func (s *record) restarting() bool {
	return s.RestartPhase == "stopping" || s.RestartPhase == "starting"
}
func (s *record) restartVersion() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s.ContainerID+"/"+s.RestartID)))
}
func (s *record) matchesMaintenance(m computer.Maintenance) bool {
	return s != nil && s.Specification.UserID == m.UserID && s.Specification.ComputerID == m.ComputerID && s.Specification.NodeID == m.NodeID && s.Specification.Generation == m.Generation && s.ContainerID == m.ContainerID
}

// ManageComputer 在独占锁内消费客户端所见版本；回包丢失或双击均不会再次停止同一电脑。
func (c *SessionController) ManageComputer(ctx context.Context, m computer.Maintenance) (receipt computer.MaintenanceReceipt, result error) {
	if m.Validate() != nil || m.NodeID != c.node.options.NodeID {
		return receipt, failure("computer_target_invalid")
	}
	if err := c.begin(); err != nil {
		return receipt, err
	}
	defer c.pending.Done()
	node := c.node
	state, err := node.journal.read(m.UserID)
	if err != nil {
		return receipt, err
	}
	if !state.matchesMaintenance(m) {
		return receipt, failure("computer_target_changed")
	}
	receipt.Target = m
	status := func() error {
		receipt.Version = state.restartVersion()
		if state.restarting() {
			receipt.State = "restarting"
			return nil
		}
		if state.RestartPhase == "failed" {
			receipt.State = "failed"
			return nil
		}
		actual, err := node.engine.inspectContainer(ctx, m.ContainerID)
		if err != nil {
			return err
		}
		if actual.ID != m.ContainerID || state.Phase != "ready" {
			return failure("computer_maintenance_unavailable")
		}
		if actual.State.Running && !actual.State.Dead {
			receipt.State = "running"
			return nil
		}
		if exitedComputer(actual) {
			receipt.State = "stopped"
			return nil
		}
		return failure("computer_maintenance_unavailable")
	}
	if m.ExpectedVersion == "" || state.restarting() {
		err = status()
		return receipt, err
	}
	release, err := node.journal.lock(ctx, m.UserID)
	if err != nil {
		return receipt, err
	}
	transferred := false
	defer func() {
		if !transferred {
			result = errors.Join(result, release())
		}
	}()
	state, err = node.journal.read(m.UserID)
	if err != nil {
		return receipt, err
	}
	if !state.matchesMaintenance(m) {
		return receipt, failure("computer_target_changed")
	}
	if state.restarting() || state.restartVersion() != m.ExpectedVersion {
		err = status()
		return receipt, err
	}
	recovering, err := node.journal.hasComputerRecovery(state)
	if err != nil {
		return receipt, err
	}
	if state.Phase != "ready" && !recovering {
		return receipt, failure("computer_maintenance_unavailable")
	}
	state.RestartID, state.RestartPhase = ids.New(), "stopping"
	if err = node.journal.save(state); err != nil {
		return receipt, err
	}
	receipt.ComputerStatus = computer.ComputerStatus{Version: state.restartVersion(), State: "restarting"}
	c.pending.Add(1)
	transferred = true
	go func() {
		defer c.pending.Done()
		bounded, stop := context.WithTimeout(context.Background(), node.restartBudget())
		defer stop()
		err := node.finishRestart(bounded, c.logger, state, func(ctx context.Context) error {
			return node.journal.visitSessions(ctx, m.UserID, func(key sessionKey) error {
				session, err := node.journal.readSession(key)
				if err != nil {
					return err
				}
				if session == nil {
					return failure("session_missing")
				}
				return c.drain(ctx, session)
			})
		})
		if err = errors.Join(err, release()); err != nil {
			logComputerRecovery(bounded, c.logger, state, err)
		}
	}()
	return receipt, nil
}
func (node *Runtime) restartBudget() time.Duration {
	return node.options.CallTimeout*3 + node.options.AgentStartup.Timeout + node.options.Browser.CleanupTimeout + node.options.Probes.CleanupTimeout + node.options.Egress.ShutdownTimeout
}
func (node *Runtime) finishRestart(ctx context.Context, logger *slog.Logger, s *record, drain func(context.Context) error) (result error) {
	res := node.withStorage(node.resource(s.Specification), s)
	defer func() {
		if result != nil {
			if s.RestartPhase == "starting" {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), node.options.Egress.ShutdownTimeout)
				result = errors.Join(result, node.egress.stop(cleanup, res))
				cancel()
			}
			s.RestartPhase = "failed"
			result = errors.Join(result, node.journal.save(s))
		}
	}()
	current, err := node.load(ctx, res, s)
	if err != nil {
		return err
	}
	if !current.hasContainer || current.container.ID != s.ContainerID {
		return failure("computer_target_changed")
	}
	if s.RestartPhase == "stopping" {
		if current.container.State.Running {
			stopErr := node.engine.stopContainer(ctx, s.ContainerID)
			current, err = node.load(ctx, res, s)
			if err != nil || !current.hasContainer || !exitedComputer(current.container) {
				return errors.Join(failure("computer_stop_unconfirmed"), stopErr, err)
			}
		}
		if !exitedComputer(current.container) {
			return failure("computer_stop_unconfirmed")
		}
		if err = drain(ctx); err != nil {
			return err
		}
		if err = node.browsers.retire(ctx, res); err != nil {
			return err
		}
		if err = node.probes.retire(ctx, res); err != nil {
			return err
		}
		if err = node.egress.stop(ctx, res); err != nil {
			return err
		}
		s.RestartPhase = "starting"
		if err = node.journal.save(s); err != nil {
			return err
		}
	}
	if err = node.resumeComputer(ctx, res, s, current); err != nil {
		return err
	}
	s.RestartPhase = ""
	if err = node.journal.save(s); err != nil {
		return err
	}
	logger.InfoContext(ctx, "用户电脑重启完成", "user_id", s.Specification.UserID, "computer_id", s.Specification.ComputerID)
	return nil
}
