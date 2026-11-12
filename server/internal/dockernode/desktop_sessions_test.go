package dockernode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

// desktopAuthority 的等待发生在替身计数锁之外，用实际重叠的调用验证桌面间没有用户级串行锁。
type desktopAuthority struct {
	*fakeAuthority
	beforeOpen  func(context.Context, computer.SessionCommand) error
	beforeClose func(context.Context, computer.SessionCommand) error
}

func (agent *desktopAuthority) open(ctx context.Context, command computer.SessionCommand, reference processgroup.Reference, deadline time.Time) error {
	if agent.beforeOpen != nil {
		if err := agent.beforeOpen(ctx, command); err != nil {
			return err
		}
	}
	return agent.fakeAuthority.open(ctx, command, reference, deadline)
}

func (agent *desktopAuthority) close(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) error {
	if agent.beforeClose != nil {
		if err := agent.beforeClose(ctx, command); err != nil {
			return err
		}
	}
	return agent.fakeAuthority.close(ctx, command, reference)
}

func otherDesktop(session computer.ExecutionSession, display int) computer.ExecutionSession {
	session.DesktopID, session.BotID = ids.New(), ids.New()
	session.SessionID, session.RunID = ids.New(), ids.New()
	session.DisplayNumber, session.UID = display, computer.DesktopUIDBase+display
	session.FencingToken = 1
	return session
}

func requireDesktopState(t *testing.T, controller *SessionController, command computer.SessionCommand, phase string) {
	t.Helper()
	state, err := controller.node.journal.readSession(sessionKeyOf(command.Session))
	if err != nil || state == nil || state.Command != command || state.Phase != phase {
		t.Fatalf("桌面记录不匹配：状态=%v，错误=%v", state, err)
	}
}

func waitDesktopCall(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("桌面调用未在期限内返回")
	}
}

func TestDesktopOperationsOverlapAndProtectLifecycle(t *testing.T) {
	for _, phase := range []string{"开启", "关闭"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			first := commandFor(fixture.prepareComputer(t, fixture.specification))
			second := commandFor(otherDesktop(first.Session, 2))
			groups := &fakeGroups{path: t.TempDir()}
			controller := fixture.sessionController(t, groups, sessionOptions())
			entered, proceed := make(chan struct{}), make(chan struct{})
			var release, arrived sync.Once
			unblock := func() { release.Do(func() { close(proceed) }) }
			defer unblock()
			gate := func(ctx context.Context, command computer.SessionCommand) error {
				if command.Session.DesktopID != first.Session.DesktopID {
					return nil
				}
				arrived.Do(func() { close(entered) })
				select {
				case <-proceed:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			agent := &desktopAuthority{fakeAuthority: &fakeAuthority{}}
			controller.agent = agent
			if phase == "开启" {
				agent.beforeOpen = gate
			} else {
				if _, err := controller.Open(ctx, first); err != nil {
					t.Fatal(err)
				}
				agent.beforeClose = gate
			}
			done := make(chan error, 1)
			go func() {
				var err error
				if phase == "开启" {
					_, err = controller.Open(ctx, first)
				} else {
					_, err = controller.Close(ctx, first)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("没有到达阻塞的代理调用")
			}
			parallel, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			defer stop()
			if _, err := controller.Open(parallel, second); err != nil {
				t.Fatal("其他桌面未能独立开启", err)
			}
			second.Revision++
			if _, err := controller.Renew(parallel, second); err != nil {
				t.Fatal("其他桌面续约被阻塞", err)
			}
			file, err := controller.Membership(parallel, second.Session, "shell")
			if err != nil {
				t.Fatal("其他桌面授权被阻塞", err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
			_, err = fixture.node.Remove(short, fixture.specification)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("生命周期未等待在途桌面调用", err)
			}
			short, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
			_, err = controller.Renew(short, first)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("同一桌面的调用没有互斥", err)
			}
			unblock()
			waitDesktopCall(t, done)
			agent.beforeOpen, agent.beforeClose = nil, nil
			requireDesktopState(t, controller, second, "active")
			if _, err = fixture.node.Remove(ctx, fixture.specification); err == nil {
				t.Fatal("另一个桌面仍活动时删除了电脑")
			}
		})
	}
}

func TestDesktopCapacityCountsSameUserAndReleasesIndividually(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	first := fixture.prepareComputer(t, fixture.specification)
	groups := &fakeGroups{path: t.TempDir()}
	options := sessionOptions()
	options.MaximumActive = 2
	controller := fixture.sessionController(t, groups, options)
	commands := []computer.SessionCommand{commandFor(first), commandFor(otherDesktop(first, 2)), commandFor(otherDesktop(first, 3)), commandFor(otherDesktop(first, 4))}
	type outcome struct {
		command computer.SessionCommand
		err     error
	}
	results, start := make(chan outcome, len(commands)), make(chan struct{})
	for _, command := range commands {
		go func() {
			<-start
			_, err := controller.Open(ctx, command)
			results <- outcome{command, err}
		}()
	}
	close(start)
	var admitted, waiting []computer.SessionCommand
	for range commands {
		result := <-results
		if result.err == nil {
			admitted = append(admitted, result.command)
		} else {
			requireFailure(t, result.err, "session_capacity")
			waiting = append(waiting, result.command)
		}
	}
	if len(admitted) != 2 || groups.creates != 2 {
		t.Fatal("没有按桌面原子计算节点容量", len(admitted), groups.creates)
	}
	before, _ := controller.lease(sessionKeyOf(admitted[1].Session))
	if _, err := controller.Close(ctx, admitted[0]); err != nil {
		t.Fatal(err)
	}
	after, ok := controller.lease(sessionKeyOf(admitted[1].Session))
	if !ok || before != after {
		t.Fatal("关闭一个桌面改变了另一个桌面的租约")
	}
	if _, err := controller.Open(ctx, waiting[0]); err != nil {
		t.Fatal("关闭桌面后没有释放一个名额", err)
	}
	_, err := controller.Open(ctx, waiting[1])
	requireFailure(t, err, "session_capacity")
	for _, command := range []computer.SessionCommand{admitted[1], waiting[0]} {
		requireDesktopState(t, controller, command, "active")
		if command.Session.FencingToken != 1 {
			t.Fatal("独立桌面没有独立控制代次")
		}
	}
}

func TestQuarantinedDesktopDoesNotRevokeOtherDesktop(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	first := commandFor(fixture.prepareComputer(t, fixture.specification))
	second := commandFor(otherDesktop(first.Session, 2))
	groups := &fakeGroups{path: t.TempDir()}
	options := sessionOptions()
	options.MaximumActive = 2
	controller := fixture.sessionController(t, groups, options)
	agent := &desktopAuthority{fakeAuthority: &fakeAuthority{}, beforeClose: func(_ context.Context, command computer.SessionCommand) error {
		if command.Session.DesktopID == first.Session.DesktopID {
			return errors.New("指定桌面关闭未确认")
		}
		return nil
	}}
	controller.agent = agent
	t.Cleanup(func() { agent.beforeClose = nil })
	for _, command := range []computer.SessionCommand{first, second} {
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := controller.Close(ctx, first); err == nil || groups.drains != 1 {
		t.Fatal("失败关闭没有独立执行宿主清理", err)
	}
	requireDesktopState(t, controller, first, "quarantined")
	second.Revision++
	if _, err := controller.Renew(ctx, second); err != nil {
		t.Fatal("其他桌面续约受到隔离影响", err)
	}
	_, err := controller.Open(ctx, commandFor(otherDesktop(first.Session, 3)))
	requireFailure(t, err, "session_capacity")
	controller.remember(sessionKeyOf(first.Session), sessionLease{deadline: time.Now().Add(-2 * time.Second)})
	controller.remember(sessionKeyOf(second.Session), sessionLease{deadline: time.Now().Add(-time.Second)})
	if count, err := controller.Reap(ctx, 2); err == nil || count != 1 {
		t.Fatal("回收单桌面失败跳过了其他桌面", count, err)
	}
	requireDesktopState(t, controller, second, "closed")
	lease, ok := controller.lease(sessionKeyOf(first.Session))
	if !ok || lease.attempt != 1 {
		t.Fatal("失败桌面未保留独立重试状态", lease)
	}
	if _, ok := controller.lease(sessionKeyOf(second.Session)); ok {
		t.Fatal("已关闭桌面仍占用活动名额")
	}
	if _, err := fixture.node.Remove(ctx, fixture.specification); err == nil {
		t.Fatal("隔离桌面未阻止整个电脑删除")
	}
}

func TestDesktopIdentityRemainsImmutableAcrossSessions(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	first := commandFor(fixture.prepareComputer(t, fixture.specification))
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	if _, err := controller.Close(ctx, first); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*computer.ExecutionSession){
		func(session *computer.ExecutionSession) { session.BotID = ids.New() },
		func(session *computer.ExecutionSession) { session.UID++; session.DisplayNumber++ },
		func(session *computer.ExecutionSession) { session.ComputerID = ids.New() },
	} {
		next := first
		next.Session.SessionID, next.Session.RunID = ids.New(), ids.New()
		next.Session.FencingToken++
		change(&next.Session)
		_, err := controller.Open(ctx, next)
		requireFailure(t, err, "session_identity_mismatch")
		requireDesktopState(t, controller, first, "closed")
	}
	if groups.creates != 0 {
		t.Fatal("身份被替换后创建了执行环境")
	}
}

func TestRestartDrainsEveryDesktopWithoutRestoringLease(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	first := commandFor(fixture.prepareComputer(t, fixture.specification))
	second := commandFor(otherDesktop(first.Session, 2))
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	for _, command := range []computer.SessionCommand{first, second} {
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	// 丢弃进程内租约来模拟死亡，持久文件仍完整保留，重启必须逐桌面回收。
	controller.mu.Lock()
	controller.closed = true
	controller.leases = map[sessionKey]sessionLease{}
	err := controller.releaseLock()
	controller.releaseLock = nil
	controller.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted := fixture.sessionController(t, groups, sessionOptions())
	for _, command := range []computer.SessionCommand{first, second} {
		requireDesktopState(t, restarted, command, "closed")
		_, err = restarted.Renew(ctx, command)
		requireFailure(t, err, "session_lease_expired")
	}
	if groups.drains != 2 || len(restarted.leases) != 0 {
		t.Fatal("重启遗漏桌面或恢复了旧授权", groups.drains)
	}
	if _, err = fixture.node.Remove(ctx, fixture.specification); err != nil {
		t.Fatal("所有桌面关闭后仍拒绝生命周期清理", err)
	}
}
