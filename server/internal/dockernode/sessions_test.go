package dockernode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

// fakeGroups 只验证节点协议和故障恢复顺序，不作为 Linux 实际进程回收证据。
type fakeGroups struct {
	mu                           sync.Mutex
	creates, drains, memberships int
	failDrain                    bool
	path                         string
}

func (groups *fakeGroups) Locate(_ context.Context, session computer.ExecutionSession, pid int) (processgroup.Reference, error) {
	if pid <= 1 {
		return processgroup.Reference{}, errors.New("无容器主进程")
	}
	return processgroup.Reference{Session: session, ParentPath: "system.slice/docker-" + session.ContainerID + ".scope", ParentDevice: 1, ParentInode: 2}, nil
}
func (groups *fakeGroups) Create(_ context.Context, reference processgroup.Reference) error {
	groups.mu.Lock()
	defer groups.mu.Unlock()
	groups.creates++
	return reference.Validate()
}
func (groups *fakeGroups) Drain(_ context.Context, reference processgroup.Reference) error {
	groups.mu.Lock()
	defer groups.mu.Unlock()
	groups.drains++
	if groups.failDrain {
		return errors.New("内核清理未确认")
	}
	return reference.Validate()
}
func (groups *fakeGroups) Membership(_ context.Context, reference processgroup.Reference, role string) (*os.File, error) {
	groups.mu.Lock()
	defer groups.mu.Unlock()
	if reference.Validate() != nil || role != "shell" && role != "browser" {
		return nil, errors.New("入组身份错误")
	}
	groups.memberships++
	return os.OpenFile(filepath.Join(groups.path, "membership"), os.O_CREATE|os.O_RDWR, 0600)
}

func (groups *fakeGroups) Controls(ctx context.Context, reference processgroup.Reference) ([]*os.File, error) {
	var files []*os.File
	for range 4 {
		file, err := groups.Membership(ctx, reference, "shell")
		if err != nil {
			for _, opened := range files {
				err = errors.Join(err, opened.Close())
			}
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}
func sessionOptions() SessionOptions {
	return SessionOptions{MaximumActive: 8, CallTimeout: time.Second, PollInterval: 10 * time.Millisecond, RetryDelay: 100 * time.Millisecond, MaxCleanupAttempts: 2}
}
func sessionLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (fixture fixture) prepareComputer(t *testing.T, specification computer.Specification) computer.ExecutionSession {
	t.Helper()
	observation, err := fixture.node.Ensure(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	fixture.engine.mu.Lock()
	name := fixture.node.resource(specification).Name
	value := fixture.engine.containers[name]
	value.State.Pid = 4242
	fixture.engine.containers[name] = value
	fixture.engine.mu.Unlock()
	return computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: specification.UserID, ComputerID: specification.ComputerID, BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: specification.NodeID, ContainerID: observation.ContainerID, Generation: specification.Generation, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
}
func (fixture fixture) sessionController(t *testing.T, groups *fakeGroups, options SessionOptions) *SessionController {
	t.Helper()
	controller, err := newSessions(context.Background(), fixture.node, groups, &fakeAuthority{}, options, sessionLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := controller.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return controller
}
func commandFor(session computer.ExecutionSession) computer.SessionCommand {
	return computer.SessionCommand{Session: session, Revision: 1, Lifetime: 5 * time.Second}
}

func TestNodeExecutionSessions(t *testing.T) {
	ctx := context.Background()
	t.Run("持久开启、重复续约不延长及旧关闭拒绝", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		observation, err := controller.Open(ctx, command)
		if err != nil || !observation.Matches(command, "active") {
			t.Fatal(observation, err)
		}
		before, _ := controller.lease(sessionKeyOf(session))
		if _, err = controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		after, _ := controller.lease(sessionKeyOf(session))
		if !before.deadline.Equal(after.deadline) || groups.creates != 1 {
			t.Fatal("重复开启延长租约或重建资源组")
		}
		command.Revision++
		if _, err = controller.Renew(ctx, command); err != nil {
			t.Fatal(err)
		}
		before, _ = controller.lease(sessionKeyOf(session))
		if _, err = controller.Renew(ctx, command); err != nil {
			t.Fatal(err)
		}
		after, _ = controller.lease(sessionKeyOf(session))
		if !before.deadline.Equal(after.deadline) {
			t.Fatal("重复续约延长期限")
		}
		old := command
		old.Revision--
		_, err = controller.Renew(ctx, old)
		requireFailure(t, err, "session_stale")
		file, err := controller.Membership(ctx, session, "shell")
		if err != nil {
			t.Fatal(err)
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.node.Remove(ctx, fixture.specification)
		requireFailure(t, err, "computer_session_active")
		command.Revision++
		if observation, err = controller.Close(ctx, command); err != nil || !observation.Matches(command, "drained") {
			t.Fatal(err)
		}
		if _, err = controller.Close(ctx, command); err != nil || groups.drains != 1 {
			t.Fatal("重复关闭再次操作旧资源组", err)
		}
		_, err = controller.Membership(ctx, session, "shell")
		requireFailure(t, err, "session_lease_expired")
		newCommand := command
		newCommand.Session.SessionID = ids.New()
		newCommand.Session.RunID = ids.New()
		newCommand.Session.FencingToken++
		newCommand.Revision = 1
		if _, err = controller.Open(ctx, newCommand); err != nil {
			t.Fatal(err)
		}
		command.Revision++
		_, err = controller.Close(ctx, command)
		requireFailure(t, err, "session_stale")
		if groups.drains != 1 {
			t.Fatal("旧关闭回收了新会话")
		}
	})
	t.Run("先关闭形成持久屏障，未知用户不能创建记录", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		command.Revision = 2
		if observation, err := controller.Close(ctx, command); err != nil || !observation.Matches(command, "drained") {
			t.Fatal(err)
		}
		_, err := controller.Open(ctx, command)
		requireFailure(t, err, "session_cannot_reopen")
		command.Session.UserID = ids.New()
		command.Session.ComputerID = ids.New()
		_, err = controller.Close(ctx, command)
		requireFailure(t, err, "session_computer_mismatch")
		if groups.creates != 0 || groups.drains != 0 {
			t.Fatal("未开启会话触发资源操作")
		}
	})
	t.Run("续约失效与回收失败均封闭入组，重试有上限", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		controller.remember(sessionKeyOf(session), sessionLease{deadline: time.Now().Add(-time.Second)})
		command.Revision++
		_, err := controller.Renew(ctx, command)
		requireFailure(t, err, "session_lease_expired")
		_, err = controller.Membership(ctx, session, "browser")
		requireFailure(t, err, "session_lease_expired")
		groups.failDrain = true
		for attempt := 1; attempt <= 2; attempt++ {
			if count, err := controller.Reap(ctx, 100); err == nil || count != 0 {
				t.Fatal("清理失败被报告成功", count, err)
			}
			lease, _ := controller.lease(sessionKeyOf(session))
			lease.retryAt = time.Now().Add(-time.Second)
			controller.remember(sessionKeyOf(session), lease)
		}
		if count, err := controller.Reap(ctx, 100); err != nil || count != 0 || groups.drains != 2 {
			t.Fatal("超过清理重试上限", count, err)
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "quarantined" {
			t.Fatal("失败会话未隔离", err)
		}
		groups.failDrain = false
		if observation, err := controller.Close(ctx, command); err != nil || !observation.Matches(command, "drained") {
			t.Fatal(err)
		}
	})
	t.Run("节点执行服务独占，关闭失败不能释放服务锁", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		if _, err := controller.Open(ctx, commandFor(session)); err != nil {
			t.Fatal(err)
		}
		short, stop := context.WithTimeout(ctx, 25*time.Millisecond)
		_, err := newSessions(short, fixture.node, groups, &fakeAuthority{}, sessionOptions(), sessionLogger())
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("并行节点未被拒绝", err)
		}
		groups.failDrain = true
		if err = controller.Shutdown(ctx); err == nil {
			t.Fatal("未清理却关闭成功")
		}
		short, stop = context.WithTimeout(ctx, 25*time.Millisecond)
		_, err = newSessions(short, fixture.node, groups, &fakeAuthority{}, sessionOptions(), sessionLogger())
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("失败关闭释放了独占锁", err)
		}
		groups.failDrain = false
		if err = controller.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		fixture.sessionController(t, groups, sessionOptions())
	})
	for _, cause := range []string{"锁等待超时", "执行记录损坏"} {
		t.Run("回收前置失败也消耗有限重试/"+cause, func(t *testing.T) {
			fixture := newFixture(t)
			session := fixture.prepareComputer(t, fixture.specification)
			groups := &fakeGroups{path: t.TempDir()}
			options := sessionOptions()
			options.RetryDelay = time.Minute
			controller := fixture.sessionController(t, groups, options)
			command := commandFor(session)
			if _, err := controller.Open(ctx, command); err != nil {
				t.Fatal(err)
			}
			controller.remember(sessionKeyOf(session), sessionLease{deadline: time.Now().Add(-time.Second)})
			if cause == "锁等待超时" {
				release, err := fixture.node.journal.lock(ctx, session.UserID)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := release(); err != nil {
						t.Error(err)
					}
				})
			} else {
				name := sessionKeyOf(session).name() + ".json"
				encoded, err := fixture.node.journal.readFile(name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := fixture.node.journal.writeFile(name, encoded); err != nil {
						t.Error(err)
					}
				})
				if err = fixture.node.journal.writeFile(name, []byte(`{"version":1}`)); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 1; attempt <= controller.options.MaxCleanupAttempts; attempt++ {
				short, stop := context.WithTimeout(ctx, 25*time.Millisecond)
				count, err := controller.Reap(short, 100)
				stop()
				if err == nil || count != 0 || groups.drains != 0 {
					t.Fatal("前置故障未保留失败状态", count, err)
				}
				lease, ok := controller.lease(sessionKeyOf(session))
				if !ok || lease.attempt != attempt || !lease.retryAt.After(time.Now()) || len(controller.due(100)) != 0 {
					t.Fatal("前置故障未计入尝试或未退避", lease)
				}
				lease.retryAt = time.Now().Add(-time.Second)
				controller.remember(sessionKeyOf(session), lease)
			}
			if count, err := controller.Reap(ctx, 100); err != nil || count != 0 || groups.drains != 0 {
				t.Fatal("前置故障超过回收次数上限", count, err)
			}
		})
	}
	t.Run("重启先回收旧会话，不按墙上时间恢复执行权", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil {
			t.Fatal(err)
		}
		state.Deadline = time.Now().Add(24 * time.Hour)
		if err = fixture.node.journal.saveSession(state); err != nil {
			t.Fatal(err)
		}
		// 模拟进程死亡：仅丢弃进程内租约并释放内核文件锁，不调用正常收尾。
		controller.mu.Lock()
		controller.closed = true
		controller.leases = map[sessionKey]sessionLease{}
		err = controller.releaseLock()
		controller.releaseLock = nil
		controller.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		opened := 0
		fixture.node.egress = &fakeEgress{ensureCall: func(context.Context, Resource, container) error {
			closed, err := fixture.node.journal.readSession(sessionKeyOf(session))
			if err != nil || closed.Phase != "closed" {
				t.Fatal("旧执行尚未关闭就恢复出网", err)
			}
			opened++
			return nil
		}}
		groups.failDrain = true
		if _, err = newSessions(ctx, fixture.node, groups, &fakeAuthority{}, sessionOptions(), sessionLogger()); err == nil {
			t.Fatal("未回收遗留进程就启动成功")
		}
		if opened != 0 {
			t.Fatal("旧会话回收失败仍恢复出网")
		}
		groups.failDrain = false
		restarted := fixture.sessionController(t, groups, sessionOptions())
		state, err = fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "closed" || groups.drains != 2 || opened != 1 {
			t.Fatal("重启未完成回收", err)
		}
		command.Revision++
		_, err = restarted.Renew(ctx, command)
		requireFailure(t, err, "session_lease_expired")
	})
	t.Run("并发用户原子预留节点容量", func(t *testing.T) {
		fixture := newFixture(t)
		first := fixture.prepareComputer(t, fixture.specification)
		other := fixture.specification
		other.UserID = ids.New()
		other.ComputerID = ids.New()
		other.OperationID = ids.New()
		other.VolumeReference = "tombot-user-" + other.UserID
		second := fixture.prepareComputer(t, other)
		groups := &fakeGroups{path: t.TempDir()}
		options := sessionOptions()
		options.MaximumActive = 1
		controller := fixture.sessionController(t, groups, options)
		results := make(chan error, 2)
		for _, session := range []computer.ExecutionSession{first, second} {
			go func() { _, err := controller.Open(ctx, commandFor(session)); results <- err }()
		}
		success := 0
		for range 2 {
			if err := <-results; err == nil {
				success++
			} else {
				requireFailure(t, err, "session_capacity")
			}
		}
		if success != 1 || groups.creates != 1 {
			t.Fatal("并发超出节点容量", success, groups.creates)
		}
	})
	t.Run("后台租约到期回收", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		command.Lifetime = time.Second
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		serve, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- controller.Serve(serve) }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			state, err := fixture.node.journal.readSession(sessionKeyOf(session))
			if err != nil {
				t.Fatal(err)
			}
			if state.Phase == "closed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("后台未在期限内回收会话")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "closed" || groups.drains != 1 {
			t.Fatal("后台未回收过期会话", err)
		}
	})
	t.Run("损坏执行记录阻止授权和生命周期删除", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		name := sessionKeyOf(session).name() + ".json"
		encoded, err := fixture.node.journal.readFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err = fixture.node.journal.writeFile(name, []byte(`{"version":1}`)); err != nil {
			t.Fatal(err)
		}
		if _, err = controller.Membership(ctx, session, "shell"); err == nil {
			t.Fatal("损坏记录仍发放执行权")
		}
		before := len(fixture.engine.events)
		if _, err = fixture.node.Remove(ctx, fixture.specification); err == nil || len(fixture.engine.events) != before {
			t.Fatal("损坏记录触发删除", err)
		}
		if err = fixture.node.journal.writeFile(name, encoded); err != nil {
			t.Fatal(err)
		}
		command.Revision++
		if _, err = controller.Close(ctx, command); err != nil {
			t.Fatal(err)
		}
	})
}
