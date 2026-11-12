package computeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type Identity struct {
	UserID     string `json:"user_id"`
	ComputerID string `json:"computer_id"`
	NodeID     string `json:"node_id"`
	Generation int64  `json:"generation"`
}

func (identity Identity) validate() error {
	for _, id := range []string{identity.UserID, identity.ComputerID, identity.NodeID} {
		if _, err := ulid.ParseStrict(id); err != nil {
			return errors.New("电脑代理实例身份无效")
		}
	}
	if identity.Generation < 1 {
		return errors.New("电脑代理实例代次无效")
	}
	return nil
}

func (identity Identity) matches(session computer.ExecutionSession) bool {
	return identity.UserID == session.UserID && identity.ComputerID == session.ComputerID && identity.NodeID == session.NodeID && identity.Generation == session.Generation
}

// Snapshot 记录不可恢复的会话关闭屏障；墙上时间不作为进程重启后的有效租约。
type Snapshot struct {
	Version int                     `json:"version"`
	Command computer.SessionCommand `json:"command"`
	Group   *processgroup.Reference `json:"group,omitempty"`
	Phase   string                  `json:"phase"`
}

func (snapshot Snapshot) validate() error {
	if snapshot.Version != 2 || snapshot.Command.Validate() != nil {
		return errors.New("电脑代理会话记录身份无效")
	}
	switch snapshot.Phase {
	case "opening", "active", "closing", "closed":
	default:
		return errors.New("电脑代理会话记录状态无效")
	}
	if snapshot.Group != nil && (snapshot.Group.Validate() != nil || snapshot.Group.Session != snapshot.Command.Session) {
		return errors.New("电脑代理会话资源组记录无效")
	}
	if snapshot.Phase != "closed" && snapshot.Group == nil {
		return errors.New("电脑代理活动会话缺少资源组")
	}
	return nil
}

type Store interface {
	Load(context.Context) (*Snapshot, error)
	Save(context.Context, Snapshot) error
}

// Scope 只持有当前资源组的限定能力。Terminate 必须确认整组为空，不能只杀直接子进程。
type Scope interface {
	Membership(string) (*os.File, error)
	Revoke() error
	Terminate(context.Context) error
	Close() error
}

// ScopeFactory 仅在成功时接管四个描述符；失败时仍由桌面守卫负责关闭，必须支持不同桌面的并发调用。
type ScopeFactory interface {
	Open(processgroup.Reference, []*os.File) (Scope, error)
}

type Options struct {
	CallTimeout     time.Duration
	CleanupTimeout  time.Duration
	RetryDelay      time.Duration
	CleanupAttempts int
}

func (options Options) validate() error {
	if options.CallTimeout < time.Second || options.CallTimeout > time.Minute || options.CleanupTimeout < time.Second || options.CleanupTimeout > time.Minute || options.RetryDelay < 10*time.Millisecond || options.RetryDelay > time.Second || options.CleanupAttempts < 1 || options.CleanupAttempts > 5 {
		return errors.New("电脑代理会话时限配置无效")
	}
	return nil
}

// desktopGuard 串行化一个稳定桌面的会话状态，通过独立计时器撤销该桌面的任务执行授权。
type desktopGuard struct {
	identity  Identity
	desktop   computer.DesktopIdentity
	capacity  *desktopCapacity
	store     Store
	factory   ScopeFactory
	options   Options
	logger    *slog.Logger
	gate      chan struct{}
	expired   chan struct{}
	state     *Snapshot
	scope     Scope
	permit    *permit
	stopped   bool
	fatal     chan struct{}
	fatalOnce sync.Once
}

// newGuard 不恢复旧进程的租约；只有更高容器代次才能封闭已被节点隔离的旧实例记录。
func newGuard(ctx context.Context, identity Identity, desktop computer.DesktopIdentity, capacity *desktopCapacity, store Store, factory ScopeFactory, options Options, logger *slog.Logger) (*desktopGuard, error) {
	if identity.validate() != nil || desktop.Validate() != nil || desktop.UserID != identity.UserID || desktop.ComputerID != identity.ComputerID || capacity == nil || store == nil || factory == nil || logger == nil || options.validate() != nil {
		return nil, errors.New("电脑代理会话配置无效")
	}
	ctx, cancel := context.WithTimeout(ctx, options.CallTimeout)
	defer cancel()
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state != nil {
		if state.validate() != nil || state.Command.Session.DesktopIdentity != desktop || state.Command.Session.UserID != identity.UserID || state.Command.Session.ComputerID != identity.ComputerID || state.Command.Session.NodeID != identity.NodeID || state.Command.Session.Generation > identity.Generation {
			return nil, errors.New("电脑代理持久实例身份不符")
		}
		if state.Phase != "closed" {
			if state.Command.Session.Generation == identity.Generation {
				return nil, errors.New("电脑代理旧会话未关闭，需要隔离当前容器")
			}
			closed := *state
			closed.Phase = "closed"
			if err = store.Save(ctx, closed); err != nil {
				return nil, err
			}
			state = &closed
		}
	}
	return &desktopGuard{identity: identity, desktop: desktop, capacity: capacity, store: store, factory: factory, options: options, logger: logger, gate: make(chan struct{}, 1), expired: make(chan struct{}, 1), fatal: make(chan struct{}), state: state}, nil
}

func (guard *desktopGuard) enter(ctx context.Context) (context.Context, func(), error) {
	return guard.acquire(ctx, false)
}

func (guard *desktopGuard) fail() { guard.fatalOnce.Do(func() { close(guard.fatal) }) }

func (guard *desktopGuard) acquire(ctx context.Context, allowFatal bool) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, guard.options.CallTimeout)
	if ctx.Err() != nil {
		cancel()
		return nil, nil, ctx.Err()
	}
	fatal := guard.fatal
	if allowFatal {
		fatal = nil
	}
	select {
	case guard.gate <- struct{}{}:
	case <-fatal:
		cancel()
		return nil, nil, errors.New("电脑代理执行环境失去可靠控制，必须退出容器")
	case <-ctx.Done():
		cancel()
		return nil, nil, ctx.Err()
	}
	select {
	case <-fatal:
		<-guard.gate
		cancel()
		return nil, nil, errors.New("电脑代理执行环境失去可靠控制，必须退出容器")
	default:
	}
	if ctx.Err() != nil {
		<-guard.gate
		cancel()
		return nil, nil, ctx.Err()
	}
	return ctx, func() { <-guard.gate; cancel() }, nil
}

func (guard *desktopGuard) validate(command computer.SessionCommand) error {
	if command.Validate() != nil || !guard.identity.matches(command.Session) || command.Session.DesktopIdentity != guard.desktop {
		return errors.New("电脑代理会话身份或期限无效")
	}
	if guard.state == nil {
		return nil
	}
	previous := guard.state.Command
	if command.Session.FencingToken < previous.Session.FencingToken || command.Session.SessionID == previous.Session.SessionID && command.Session != previous.Session || command.Session.FencingToken == previous.Session.FencingToken && command.Session != previous.Session || command.Session == previous.Session && (command.Revision < previous.Revision || command.Revision == previous.Revision && command.Lifetime != previous.Lifetime) {
		return errors.New("电脑代理拒绝旧控制版本或替换身份")
	}
	if command.Session != previous.Session && guard.state.Phase != "closed" {
		return errors.New("电脑代理前一会话尚未关闭")
	}
	return nil
}

func agentObservation(command computer.SessionCommand, phase string) computer.SessionObservation {
	return computer.SessionObservation{Session: command.Session, Revision: command.Revision, State: phase}
}

// Open 接管传入描述符，不启动用户程序。重复版本只确认现有期限，不延长授权。
func (guard *desktopGuard) Open(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, files []*os.File) (observation computer.SessionObservation, failure error) {
	return guard.openAt(ctx, command, reference, files, time.Now().Add(command.Lifetime))
}

func (guard *desktopGuard) openAt(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, files []*os.File, deadline time.Time) (observation computer.SessionObservation, failure error) {
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	ctx, leave, err := guard.enter(ctx)
	if err != nil {
		return observation, err
	}
	defer leave()
	if guard.stopped {
		return observation, errors.New("电脑代理已停止接收执行会话")
	}
	if err = guard.validate(command); err != nil {
		return observation, err
	}
	if reference.Validate() != nil || reference.Session != command.Session || len(files) != 4 {
		return observation, errors.New("电脑代理开启资源组身份无效")
	}
	if guard.state != nil && guard.state.Command.Session == command.Session {
		if guard.state.Phase != "active" || guard.state.Group == nil || *guard.state.Group != reference || guard.state.Command.Revision != command.Revision || guard.permit == nil || !guard.permit.active(command.Revision) {
			return observation, errors.New("电脑代理会话不能重新开启")
		}
		return agentObservation(command, "active"), nil
	}
	if !time.Now().Before(deadline) {
		return observation, errors.New("电脑代理开启期限已耗尽")
	}
	if err = guard.capacity.reserve(guard.desktop.DesktopID); err != nil {
		return observation, err
	}
	scope, err := guard.factory.Open(reference, files)
	if err != nil {
		guard.capacity.release(guard.desktop.DesktopID)
		return observation, err
	}
	if scope == nil {
		guard.capacity.release(guard.desktop.DesktopID)
		return observation, errors.New("电脑代理缺少实际资源组控制能力")
	}
	files = nil
	permit, err := newPermit(deadline, command.Revision, guard.expired, func() {
		go func() {
			if err := scope.Revoke(); err != nil {
				guard.logger.Error("电脑代理独立终止信号失败，必须退出容器", "user_id", guard.identity.UserID, "computer_id", guard.identity.ComputerID, "desktop_id", guard.desktop.DesktopID, "session_id", command.Session.SessionID)
				guard.fail()
			}
		}()
	})
	if err != nil {
		closeErr := scope.Close()
		if closeErr == nil {
			guard.capacity.release(guard.desktop.DesktopID)
		} else {
			guard.fail()
		}
		return observation, errors.Join(err, closeErr)
	}
	guard.scope, guard.permit = scope, permit
	guard.state = &Snapshot{Version: 2, Command: command, Group: &reference, Phase: "opening"}
	if err = guard.store.Save(ctx, *guard.state); err != nil {
		return observation, errors.Join(err, permit.revoke())
	}
	active := *guard.state
	active.Phase = "active"
	if err = guard.store.Save(ctx, active); err != nil {
		return observation, errors.Join(err, permit.revoke())
	}
	guard.state = &active
	if !permit.active(command.Revision) {
		return observation, errors.New("电脑代理开启期间执行授权已撤销")
	}
	return agentObservation(command, "active"), nil
}

// Renew 先持久记录新版本，随后续约仍未过期的本地授权；过期或关闭的管道永不复活。
func (guard *desktopGuard) Renew(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	return guard.renewAt(ctx, command, time.Now().Add(command.Lifetime))
}

func (guard *desktopGuard) renewAt(ctx context.Context, command computer.SessionCommand, deadline time.Time) (computer.SessionObservation, error) {
	ctx, leave, err := guard.enter(ctx)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	defer leave()
	if err = guard.validate(command); err != nil {
		return computer.SessionObservation{}, err
	}
	if guard.stopped || guard.state == nil || guard.state.Command.Session != command.Session || guard.state.Phase != "active" || guard.permit == nil || !guard.permit.active(guard.state.Command.Revision) {
		return computer.SessionObservation{}, errors.New("电脑代理执行授权已结束")
	}
	if command.Revision == guard.state.Command.Revision {
		return agentObservation(command, "active"), nil
	}
	renewed := *guard.state
	renewed.Command = command
	// 保存失败可能发生在 rename 后；先保留最高受理版本，不能用旧关闭请求覆盖未知的新版本。
	guard.state = &renewed
	if err = guard.store.Save(ctx, renewed); err != nil {
		return computer.SessionObservation{}, errors.Join(err, guard.permit.revoke())
	}
	if err = guard.permit.renew(command.Revision, deadline); err != nil {
		return computer.SessionObservation{}, err
	}
	return agentObservation(command, "active"), nil
}

// WithTask 把受限入组与撤销读端借给一次受信启动回调；回调不得保留或关闭描述符。
// 回调只能启动 computer-task，不能直接执行用户程序。计时器不等待该回调即可撤销授权。
func (guard *desktopGuard) WithTask(ctx context.Context, session computer.ExecutionSession, role string, start func(context.Context, processgroup.Reference, *os.File, *os.File) error) (failure error) {
	ctx, leave, err := guard.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	if start == nil || session.Validate() != nil || !guard.identity.matches(session) || guard.stopped || guard.state == nil || guard.state.Command.Session != session || guard.state.Phase != "active" || guard.scope == nil || guard.permit == nil || !guard.permit.active(guard.state.Command.Revision) {
		return errors.New("电脑代理任务没有有效执行授权")
	}
	file, err := guard.scope.Membership(role)
	if err != nil {
		return err
	}
	defer func() {
		if file.Close() != nil {
			failure = errors.Join(failure, errors.New("关闭电脑代理任务入组描述符失败"))
		}
	}()
	if !guard.permit.active(guard.state.Command.Revision) {
		return errors.New("电脑代理任务入组前授权已结束")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = start(ctx, *guard.state.Group, file, guard.permit.reader)
	if !guard.permit.active(guard.state.Command.Revision) {
		return errors.Join(err, errors.New("电脑代理任务启动期间授权已结束，结果需要对账"))
	}
	return err
}

// Close 先撤销迟到引导，再终止整组并持久关闭屏障；不把单次空组观测当成宿主交接完成。
func (guard *desktopGuard) Close(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	ctx, leave, err := guard.enter(ctx)
	if err != nil {
		return computer.SessionObservation{}, err
	}
	defer leave()
	if err = guard.validate(command); err != nil {
		return computer.SessionObservation{}, err
	}
	if guard.state == nil || guard.state.Command.Session != command.Session {
		state := Snapshot{Version: 2, Command: command, Phase: "closed"}
		guard.state = &state
		if err = guard.store.Save(ctx, state); err != nil {
			return computer.SessionObservation{}, err
		}
		return agentObservation(command, "closed"), nil
	}
	if guard.state.Phase == "closed" {
		state := *guard.state
		state.Command = command
		guard.state = &state
		if err = guard.store.Save(ctx, state); err != nil {
			return computer.SessionObservation{}, err
		}
		return agentObservation(command, "closed"), nil
	}
	if err = guard.retire(ctx, command); err != nil {
		return computer.SessionObservation{}, err
	}
	return agentObservation(command, "closed"), nil
}

func (guard *desktopGuard) retire(ctx context.Context, command computer.SessionCommand) error {
	if guard.permit == nil || guard.scope == nil {
		return errors.New("电脑代理缺少未关闭会话的清理能力")
	}
	revokeErr := guard.permit.revoke()
	closing := *guard.state
	closing.Phase, closing.Command = "closing", command
	guard.state = &closing
	storeErr := guard.store.Save(ctx, closing)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), guard.options.CleanupTimeout)
	defer cancel()
	if err := guard.scope.Terminate(cleanup); err != nil {
		return errors.Join(revokeErr, storeErr, err)
	}
	if revokeErr != nil {
		return errors.Join(revokeErr, storeErr)
	}
	if err := guard.scope.Close(); err != nil {
		return errors.Join(revokeErr, storeErr, err)
	}
	if err := guard.permit.close(); err != nil {
		return errors.Join(revokeErr, storeErr, err)
	}
	closed := closing
	closed.Phase = "closed"
	if err := guard.store.Save(cleanup, closed); err != nil {
		return errors.Join(revokeErr, storeErr, err)
	}
	guard.state, guard.scope, guard.permit = &closed, nil, nil
	guard.capacity.release(guard.desktop.DesktopID)
	return errors.Join(revokeErr, storeErr)
}

// Serve 在授权到期后有限重试整组回收；耗尽返回错误，容器入口必须退出以终止整个 PID 命名空间。
func (guard *desktopGuard) Serve(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-guard.fatal:
			return errors.New("电脑代理失去执行环境控制，必须退出容器")
		case <-guard.expired:
		}
		for attempt := 1; attempt <= guard.options.CleanupAttempts; attempt++ {
			err := guard.cleanupExpired(ctx, attempt)
			if err == nil {
				break
			}
			if attempt == guard.options.CleanupAttempts {
				guard.fail()
				return errors.New("电脑代理无法确认执行环境已清理，必须退出容器")
			}
			timer := time.NewTimer(guard.options.RetryDelay * time.Duration(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-guard.fatal:
				timer.Stop()
				return errors.New("电脑代理失去执行环境控制，必须退出容器")
			case <-timer.C:
			}
		}
	}
}

// cleanupExpired 不让不可取消的本地文件系统调用阻止看门狗向入口报告致命故障。
// 期限耗尽后禁止再接收操作；入口必须退出，不能继续复用仍有清理协程的代理。
func (guard *desktopGuard) cleanupExpired(ctx context.Context, attempt int) error {
	bounded, cancel := context.WithTimeout(ctx, guard.options.CallTimeout+guard.options.CleanupTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- guard.reapExpired(bounded, attempt) }()
	select {
	case err := <-result:
		return err
	case <-guard.fatal:
		return errors.New("电脑代理独立终止失败，必须退出容器")
	case <-ctx.Done():
		return ctx.Err()
	case <-bounded.Done():
		guard.fail()
		return errors.New("电脑代理资源回收或记录提交阻塞，必须退出容器")
	}
}

func (guard *desktopGuard) reapExpired(ctx context.Context, attempt int) error {
	call, leave, err := guard.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	if guard.state == nil || guard.state.Phase == "closed" || guard.permit != nil && guard.state.Phase == "active" && guard.permit.active(guard.state.Command.Revision) {
		return nil
	}
	err = guard.retire(call, guard.state.Command)
	if err != nil {
		guard.logger.WarnContext(ctx, "电脑代理执行授权已结束，资源回收尚未确认", "user_id", guard.identity.UserID, "computer_id", guard.identity.ComputerID, "desktop_id", guard.desktop.DesktopID, "session_id", guard.state.Command.Session.SessionID, "attempt", attempt)
	} else {
		guard.logger.InfoContext(ctx, "电脑代理过期执行会话已回收", "user_id", guard.identity.UserID, "computer_id", guard.identity.ComputerID, "desktop_id", guard.desktop.DesktopID, "session_id", guard.state.Command.Session.SessionID)
	}
	return err
}

// Shutdown 封闭新会话并收尾当前授权；失败不能被入口忽略后继续运行。
func (guard *desktopGuard) Shutdown(ctx context.Context) error {
	ctx, leave, err := guard.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer leave()
	guard.stopped = true
	if guard.state == nil || guard.state.Phase == "closed" {
		return nil
	}
	return guard.retire(ctx, guard.state.Command)
}
