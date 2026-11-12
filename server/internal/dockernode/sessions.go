package dockernode

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type SessionOptions struct {
	MaximumActive                         int
	CallTimeout, PollInterval, RetryDelay time.Duration
	MaxCleanupAttempts                    int
}

// Validate 供部署入口先检查会话时限与容量，不打开资源组或持久记录。
func (options SessionOptions) Validate() error {
	if options.MaximumActive < 1 || options.MaximumActive > 10000 || options.CallTimeout < time.Second || options.CallTimeout > time.Minute || options.PollInterval < 10*time.Millisecond || options.PollInterval > time.Second || options.RetryDelay < 100*time.Millisecond || options.RetryDelay > time.Minute || options.MaxCleanupAttempts < 1 || options.MaxCleanupAttempts > 10 {
		return errors.New("电脑执行节点配置无效")
	}
	return nil
}

var errSessionControllerClosed = errors.New("电脑执行节点已停止接收请求")

type sessionLease struct {
	deadline time.Time
	retryAt  time.Time
	attempt  int
}

// SessionController 按桌面持久化并互斥，共享用户生命周期读锁，独占节点执行服务锁。进程重启后先回收旧会话，再接受新命令。
type SessionController struct {
	node        *Runtime
	groups      processgroup.Groups
	agent       sessionAuthority
	options     SessionOptions
	logger      *slog.Logger
	epoch       string
	mu          sync.Mutex
	leases      map[sessionKey]sessionLease
	closed      bool
	pending     sync.WaitGroup
	releaseLock func() error
}

// NewSessions 要求真实资源组后端；启动恢复失败时拒绝提供执行服务，不按持久墙上时间恢复旧租约。
func NewSessions(ctx context.Context, node *Runtime, groups processgroup.Groups, options SessionOptions, logger *slog.Logger) (controller *SessionController, result error) {
	return newSessions(ctx, node, groups, &agentLink{node: node, groups: groups, timeout: options.CallTimeout}, options, logger)
}

func newSessions(ctx context.Context, node *Runtime, groups processgroup.Groups, agent sessionAuthority, options SessionOptions, logger *slog.Logger) (controller *SessionController, result error) {
	if node == nil || node.journal == nil || groups == nil || agent == nil || logger == nil || options.Validate() != nil {
		return nil, errors.New("电脑执行节点配置无效")
	}
	ctx, stop := context.WithTimeout(ctx, options.CallTimeout)
	defer stop()
	release, err := node.journal.lock(ctx, "execution-service")
	if err != nil {
		return nil, err
	}
	controller = &SessionController{node: node, groups: groups, agent: agent, options: options, logger: logger, epoch: ids.New(), leases: map[sessionKey]sessionLease{}, releaseLock: release}
	if err = controller.recover(ctx); err != nil {
		return nil, errors.Join(err, release())
	}
	// 先终止所有旧执行，再恢复电脑出网，避免节点重启给未清理进程重新开放副作用通道。
	if err = node.restoreEgress(ctx); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), node.options.Egress.ShutdownTimeout)
		defer cancel()
		return nil, errors.Join(err, node.egress.shutdown(cleanup), release())
	}
	return controller, nil
}

func (controller *SessionController) reserve(key sessionKey, lease sessionLease) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if _, exists := controller.leases[key]; !exists && len(controller.leases) >= controller.options.MaximumActive {
		return failure("session_capacity")
	}
	controller.leases[key] = lease
	return nil
}

func (controller *SessionController) begin() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closed {
		return errSessionControllerClosed
	}
	controller.pending.Add(1)
	return nil
}
func (controller *SessionController) lease(key sessionKey) (sessionLease, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	lease, ok := controller.leases[key]
	return lease, ok
}
func (controller *SessionController) remember(key sessionKey, lease sessionLease) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.leases[key] = lease
}
func (controller *SessionController) forget(key sessionKey) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	delete(controller.leases, key)
}

func (controller *SessionController) withLock(ctx context.Context, command computer.SessionCommand, apply func(context.Context, *sessionRecord) (computer.SessionObservation, error)) (observation computer.SessionObservation, result error) {
	if command.Validate() != nil || command.Session.NodeID != controller.node.options.NodeID {
		return observation, failure("session_invalid")
	}
	bounded, stop := context.WithTimeout(ctx, controller.options.CallTimeout)
	defer stop()
	release, err := controller.node.journal.lockSession(bounded, sessionKeyOf(command.Session))
	if err != nil {
		return observation, err
	}
	defer func() { result = errors.Join(result, release()) }()
	state, err := controller.node.journal.readSession(sessionKeyOf(command.Session))
	if err != nil {
		return observation, err
	}
	if state != nil {
		previous := state.Command
		if command.Session.DesktopIdentity != previous.Session.DesktopIdentity {
			return observation, failure("session_identity_mismatch")
		}
		if command.Session.ComputerID != previous.Session.ComputerID || command.Session.NodeID != previous.Session.NodeID || command.Session.Generation < previous.Session.Generation || command.Session.FencingToken < previous.Session.FencingToken {
			return observation, failure("session_stale")
		}
		if command.Session.FencingToken == previous.Session.FencingToken && command.Session != previous.Session {
			return observation, failure("session_identity_mismatch")
		}
		if command.Session.SessionID == previous.Session.SessionID && (command.Session != previous.Session || command.Revision < previous.Revision || command.Revision == previous.Revision && command.Lifetime != previous.Lifetime) {
			return observation, failure("session_stale")
		}
		if command.Session != previous.Session && state.Phase != "closed" {
			return observation, failure("session_not_released")
		}
	}
	return apply(bounded, state)
}

func sessionObserved(command computer.SessionCommand, state string) computer.SessionObservation {
	return computer.SessionObservation{Session: command.Session, Revision: command.Revision, State: state}
}

// Open 先持久记录开启意图和父资源组身份，再创建空资源组。重复请求不延长租约或重做未确认开启。
func (controller *SessionController) Open(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	if err := controller.begin(); err != nil {
		return computer.SessionObservation{}, err
	}
	defer controller.pending.Done()
	deadline := computer.LeaseDeadline(ctx, command.Lifetime)
	return controller.withLock(ctx, command, func(ctx context.Context, state *sessionRecord) (computer.SessionObservation, error) {
		if !time.Now().Before(deadline) {
			return computer.SessionObservation{}, failure("session_lease_expired")
		}
		if state != nil && state.Command.Session == command.Session {
			lease, ok := controller.lease(sessionKeyOf(command.Session))
			if state.Phase != "active" || state.Epoch != controller.epoch || !ok || !time.Now().Before(lease.deadline) || state.Command.Revision != command.Revision {
				return computer.SessionObservation{}, failure("session_cannot_reopen")
			}
			return sessionObserved(command, "active"), nil
		}
		resourceState, err := controller.node.journal.read(command.Session.UserID)
		if err != nil {
			return computer.SessionObservation{}, err
		}
		if resourceState == nil || resourceState.Phase != "ready" || resourceState.Specification.ComputerID != command.Session.ComputerID || resourceState.Specification.NodeID != command.Session.NodeID || resourceState.Specification.Generation != command.Session.Generation || resourceState.ContainerID != command.Session.ContainerID {
			return computer.SessionObservation{}, failure("session_computer_mismatch")
		}
		resource := controller.node.resource(resourceState.Specification)
		current, err := controller.node.load(ctx, resource, resourceState)
		if err != nil {
			return computer.SessionObservation{}, err
		}
		if !current.hasContainer || !current.container.State.Running || current.container.State.Dead || current.container.State.Pid <= 1 {
			return computer.SessionObservation{}, failure("session_computer_stopped")
		}
		state = &sessionRecord{Version: 2, Command: command, Epoch: controller.epoch, Phase: "opening", Deadline: deadline.UTC()}
		if err = controller.reserve(sessionKeyOf(command.Session), sessionLease{deadline: deadline}); err != nil {
			return computer.SessionObservation{}, err
		}
		if err = controller.node.journal.saveSession(state); err != nil {
			return computer.SessionObservation{}, err
		}
		reference, err := controller.groups.Locate(ctx, command.Session, current.container.State.Pid)
		if err != nil {
			return computer.SessionObservation{}, err
		}
		if reference.Session != command.Session || reference.Validate() != nil {
			return computer.SessionObservation{}, failure("session_group_identity")
		}
		state.Group = &reference
		if err = controller.node.journal.saveSession(state); err != nil {
			return computer.SessionObservation{}, err
		}
		if err = controller.groups.Create(ctx, reference); err != nil {
			return computer.SessionObservation{}, err
		}
		if !time.Now().Before(deadline) {
			return computer.SessionObservation{}, failure("session_lease_expired")
		}
		if err = controller.agent.open(ctx, command, reference, deadline); err != nil {
			controller.remember(sessionKeyOf(command.Session), sessionLease{deadline: time.Now()})
			return computer.SessionObservation{}, err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			controller.remember(sessionKeyOf(command.Session), sessionLease{deadline: time.Now()})
			return computer.SessionObservation{}, failure("session_agent_confirmation_expired")
		}
		state.Phase = "active"
		if err = controller.node.journal.saveSession(state); err != nil {
			return computer.SessionObservation{}, err
		}
		return sessionObserved(command, "active"), nil
	})
}

// Renew 只更新本进程持有的有效单调时钟期限；同版本重放不会延长租约，关闭后的更高版本也不能复活会话。
func (controller *SessionController) Renew(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	if err := controller.begin(); err != nil {
		return computer.SessionObservation{}, err
	}
	defer controller.pending.Done()
	deadline := computer.LeaseDeadline(ctx, command.Lifetime)
	return controller.withLock(ctx, command, func(ctx context.Context, state *sessionRecord) (computer.SessionObservation, error) {
		lease, ok := controller.lease(sessionKeyOf(command.Session))
		if state == nil || state.Command.Session != command.Session || state.Phase != "active" || state.Epoch != controller.epoch || !ok || !time.Now().Before(lease.deadline) || !time.Now().Before(deadline) {
			return computer.SessionObservation{}, failure("session_lease_expired")
		}
		if command.Revision == state.Command.Revision {
			return sessionObserved(command, "active"), nil
		}
		state.Command = command
		state.Deadline = deadline.UTC()
		if err := controller.node.journal.saveSession(state); err != nil {
			return computer.SessionObservation{}, err
		}
		if err := controller.agent.renew(ctx, command, *state.Group, deadline); err != nil {
			state.Phase = "quarantined"
			controller.remember(sessionKeyOf(command.Session), sessionLease{deadline: time.Now()})
			return computer.SessionObservation{}, errors.Join(err, controller.node.journal.saveSession(state))
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			state.Phase = "quarantined"
			controller.remember(sessionKeyOf(command.Session), sessionLease{deadline: time.Now()})
			return computer.SessionObservation{}, errors.Join(failure("session_agent_confirmation_expired"), controller.node.journal.saveSession(state))
		}
		controller.remember(sessionKeyOf(command.Session), sessionLease{deadline: deadline})
		return sessionObserved(command, "active"), nil
	})
}

// Close 先刷盘封闭输入，再回收并移除完整资源组；未收到过 Open 时也保存关闭屏障，拒绝迟到开启。
func (controller *SessionController) Close(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	if err := controller.begin(); err != nil {
		return computer.SessionObservation{}, err
	}
	defer controller.pending.Done()
	return controller.withLock(ctx, command, func(ctx context.Context, state *sessionRecord) (computer.SessionObservation, error) {
		if state == nil || state.Command.Session != command.Session {
			resource, err := controller.node.journal.read(command.Session.UserID)
			if err != nil {
				return computer.SessionObservation{}, err
			}
			if resource == nil || resource.Specification.ComputerID != command.Session.ComputerID || resource.Specification.NodeID != command.Session.NodeID || resource.Specification.Generation != command.Session.Generation || resource.ContainerID != command.Session.ContainerID {
				return computer.SessionObservation{}, failure("session_computer_mismatch")
			}
			// 未收到开启也可能遇到关闭结果丢失；先纳入回收与停服集合，不能留下无人追踪的屏障。
			if err = controller.reserve(sessionKeyOf(command.Session), sessionLease{deadline: time.Now()}); err != nil {
				return computer.SessionObservation{}, err
			}
			state = &sessionRecord{Version: 2, Command: command, Epoch: controller.epoch, Phase: "closing", Deadline: time.Now().UTC()}
		}
		state.Command = command
		if err := controller.drain(ctx, state); err != nil {
			return computer.SessionObservation{}, err
		}
		return sessionObserved(command, "drained"), nil
	})
}

func (controller *SessionController) drain(ctx context.Context, state *sessionRecord) error {
	if state.Phase == "closed" {
		if err := controller.node.journal.saveSession(state); err != nil {
			return err
		}
		controller.forget(sessionKeyOf(state.Command.Session))
		return nil
	}
	state.Phase = "closing"
	state.Epoch = controller.epoch
	if err := controller.node.journal.saveSession(state); err != nil {
		return err
	}
	// 即使代理失联，也必须尝试宿主整组清理；只有两端均确认才能交接新会话。
	agentErr := controller.agent.close(ctx, state.Command, state.Group)
	if state.Group != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), controller.options.CallTimeout)
		defer stop()
		if err := controller.groups.Drain(cleanup, *state.Group); err != nil {
			state.Phase = "quarantined"
			return errors.Join(agentErr, err, controller.node.journal.saveSession(state))
		}
	}
	if agentErr != nil {
		state.Phase = "quarantined"
		return errors.Join(agentErr, controller.node.journal.saveSession(state))
	}
	state.Phase = "closed"
	if err := controller.node.journal.saveSession(state); err != nil {
		return err
	}
	controller.forget(sessionKeyOf(state.Command.Session))
	return nil
}

// Membership 在受桌面锁保护的活动会话中发放入组描述符；调用者只能交给受信容器引导程序。
func (controller *SessionController) Membership(ctx context.Context, session computer.ExecutionSession, role string) (file *os.File, result error) {
	result = controller.withActiveSession(ctx, session, func(ctx context.Context, state *sessionRecord, _ sessionLease) error {
		var err error
		file, err = controller.groups.Membership(ctx, *state.Group, role)
		return err
	})
	if result != nil && file != nil {
		result = errors.Join(result, file.Close())
		file = nil
	}
	return file, result
}

// withActiveSession 是入组、描述符委派和续约同步共同使用的活动执行权检查。
func (controller *SessionController) withActiveSession(ctx context.Context, session computer.ExecutionSession, apply func(context.Context, *sessionRecord, sessionLease) error) (result error) {
	if err := controller.begin(); err != nil {
		return err
	}
	defer controller.pending.Done()
	if session.Validate() != nil || session.NodeID != controller.node.options.NodeID {
		return failure("session_invalid")
	}
	ctx, stop := context.WithTimeout(ctx, controller.options.CallTimeout)
	defer stop()
	release, err := controller.node.journal.lockSession(ctx, sessionKeyOf(session))
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, release())
	}()
	state, err := controller.node.journal.readSession(sessionKeyOf(session))
	if err != nil {
		return err
	}
	lease, ok := controller.lease(sessionKeyOf(session))
	if state == nil || state.Command.Session != session || state.Phase != "active" || state.Epoch != controller.epoch || state.Group == nil || !ok || !time.Now().Before(lease.deadline) {
		return failure("session_lease_expired")
	}
	return apply(ctx, state, lease)
}

// Serve 独立回收失去续约的会话；终止服务后仍必须调用 Shutdown 完成收尾并释放节点独占锁。
func (controller *SessionController) Serve(ctx context.Context) error {
	ticker := time.NewTicker(controller.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := controller.Reap(ctx, 100); errors.Is(err, errSessionControllerClosed) {
				return err
			} else if err != nil && ctx.Err() == nil {
				controller.logger.WarnContext(ctx, "电脑执行会话回收尚未全部确认", "node_id", controller.node.options.NodeID)
			}
		}
	}
}

func (controller *SessionController) due(limit int) []sessionKey {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	type pending struct {
		id sessionKey
		at time.Time
	}
	values := []pending{}
	at := time.Now()
	for id, lease := range controller.leases {
		if !at.Before(lease.deadline) && !at.Before(lease.retryAt) && lease.attempt < controller.options.MaxCleanupAttempts {
			values = append(values, pending{id, lease.deadline})
		}
	}
	slices.SortFunc(values, func(a, b pending) int { return a.at.Compare(b.at) })
	result := make([]sessionKey, 0, min(limit, len(values)))
	for _, value := range values[:min(limit, len(values))] {
		result = append(result, value.id)
	}
	return result
}

// Reap 每次至多处理 100 个失效会话，清理失败有限退避并保留隔离；单桌面失败不会跳过其他桌面的回收。
func (controller *SessionController) Reap(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("电脑会话回收批量无效")
	}
	if err := controller.begin(); err != nil {
		return 0, err
	}
	defer controller.pending.Done()
	count := 0
	var failures error
	for _, key := range controller.due(limit) {
		closed, err := controller.reapDesktop(ctx, key, false)
		if err != nil {
			controller.logger.WarnContext(ctx, "电脑会话回收失败，继续保持隔离", "user_id", key.userID, "desktop_id", key.desktopID)
			failures = errors.Join(failures, err)
		} else if closed {
			count++
		}
		if ctx.Err() != nil {
			break
		}
	}
	return count, failures
}

// reserveReap 在等待文件锁和读取记录之前计入尝试，避免锁故障或损坏记录绕过退避上限。
func (controller *SessionController) reserveReap(key sessionKey, force bool) (sessionLease, bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	lease, ok := controller.leases[key]
	at := time.Now()
	if !force && (!ok || at.Before(lease.deadline) || at.Before(lease.retryAt) || lease.attempt >= controller.options.MaxCleanupAttempts) {
		return sessionLease{}, false
	}
	lease.attempt++
	lease.retryAt = at.Add(min(controller.options.RetryDelay*time.Duration(1<<min(lease.attempt-1, 6)), time.Minute))
	controller.leases[key] = lease
	return lease, true
}

func (controller *SessionController) reapDesktop(ctx context.Context, key sessionKey, force bool) (closed bool, result error) {
	reserved, ok := controller.reserveReap(key, force)
	if !ok {
		return false, nil
	}
	ctx, stop := context.WithTimeout(ctx, controller.options.CallTimeout)
	defer stop()
	release, err := controller.node.journal.lockSession(ctx, key)
	if err != nil {
		return false, err
	}
	defer func() { result = errors.Join(result, release()) }()
	lease, ok := controller.lease(key)
	// 等锁期间可能完成关闭、开启下一会话或产生更新的回收尝试，旧预约不能作用于新状态。
	if !ok || lease != reserved {
		return false, nil
	}
	state, err := controller.node.journal.readSession(key)
	if err != nil {
		return false, err
	}
	if state == nil || state.Phase == "closed" {
		controller.forget(key)
		return false, nil
	}
	if state.Command.Session.NodeID != controller.node.options.NodeID {
		return false, failure("session_node_mismatch")
	}
	if err = controller.drain(ctx, state); err != nil {
		return false, err
	}
	return true, nil
}

// Shutdown 停止新请求并等候在途调用，再回收全部本进程会话；未确认清理时保持锁，允许显式重试收尾。
func (controller *SessionController) Shutdown(ctx context.Context) error {
	controller.mu.Lock()
	controller.closed = true
	controller.mu.Unlock()
	done := make(chan struct{})
	go func() { controller.pending.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	controller.mu.Lock()
	desktops := make([]sessionKey, 0, len(controller.leases))
	for id := range controller.leases {
		desktops = append(desktops, id)
	}
	controller.mu.Unlock()
	var failures error
	for _, id := range desktops {
		_, err := controller.reapDesktop(ctx, id, true)
		failures = errors.Join(failures, err)
	}
	if failures != nil {
		return failures
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.releaseLock != nil {
		err := controller.releaseLock()
		controller.releaseLock = nil
		return err
	}
	return nil
}

var _ computer.SessionRuntime = (*SessionController)(nil)
