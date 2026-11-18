package dockernode

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type fakeAuthority struct {
	mu                             sync.Mutex
	opens, renewals, closes        int
	failOpen, failRenew, failClose bool
	deadline                       time.Time
	closedBeforeDrain              func() error
}

func TestNodePreservesReceivedLeaseDeadline(t *testing.T) {
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	agent := controller.agent.(*fakeAuthority)
	command := commandFor(session)
	deadline := time.Now().Add(time.Second)
	ctx := computer.WithLeaseDeadline(context.Background(), deadline)
	if _, err := controller.Open(ctx, command); err != nil {
		t.Fatal(err)
	}
	lease, _ := controller.lease(sessionKeyOf(session))
	if !lease.deadline.Equal(deadline) || !agent.deadline.Equal(deadline) {
		t.Fatal("开启时重新获得了完整租期")
	}
	next := time.Now().Add(1500 * time.Millisecond)
	command.Revision++
	if _, err := controller.Renew(computer.WithLeaseDeadline(context.Background(), next), command); err != nil {
		t.Fatal(err)
	}
	lease, _ = controller.lease(sessionKeyOf(session))
	if !lease.deadline.Equal(next) || !agent.deadline.Equal(next) {
		t.Fatal("续约时重新获得了完整租期")
	}
	if _, err := controller.Renew(computer.WithLeaseDeadline(context.Background(), time.Now().Add(3*time.Second)), command); err != nil {
		t.Fatal(err)
	}
	lease, _ = controller.lease(sessionKeyOf(session))
	if !lease.deadline.Equal(next) || agent.renewals != 1 {
		t.Fatal("重复版本延长了实际租约")
	}
	command.Revision++
	if _, err := controller.Renew(computer.WithLeaseDeadline(context.Background(), time.Now().Add(-time.Millisecond)), command); err == nil {
		t.Fatal("已过期的传输租约被重新激活")
	}
	if agent.renewals != 1 {
		t.Fatal("过期租约到达了代理")
	}
}

func (agent *fakeAuthority) open(_ context.Context, command computer.SessionCommand, reference processgroup.Reference, deadline time.Time) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.opens++
	agent.deadline = deadline
	if command.Session != reference.Session || !deadline.After(time.Now()) || agent.failOpen {
		return errors.New("代理开启未确认")
	}
	return nil
}
func (agent *fakeAuthority) renew(_ context.Context, command computer.SessionCommand, reference processgroup.Reference, deadline time.Time) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.renewals++
	agent.deadline = deadline
	if command.Session != reference.Session || !deadline.After(time.Now()) || agent.failRenew {
		return errors.New("代理续约未确认")
	}
	return nil
}
func (agent *fakeAuthority) close(_ context.Context, _ computer.SessionCommand, _ *processgroup.Reference) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.closes++
	if agent.closedBeforeDrain != nil {
		if err := agent.closedBeforeDrain(); err != nil {
			return err
		}
	}
	if agent.failClose {
		return errors.New("代理关闭未确认")
	}
	return nil
}

func TestNodeAgentAcknowledgementAndRecovery(t *testing.T) {
	ctx := context.Background()
	t.Run("代理确认前不开放会话", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		agent := controller.agent.(*fakeAuthority)
		agent.failOpen = true
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err == nil {
			t.Fatal("代理拒绝却返回开启成功")
		}
		if file, err := controller.Membership(ctx, session, "shell"); err == nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("未确认授权仍可入组")
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "opening" {
			t.Fatal(state, err)
		}
		agent.failOpen = false
		if _, err = controller.Open(ctx, command); err == nil || agent.opens != 1 {
			t.Fatal("未知开启被自动重发", err)
		}
		if count, err := controller.Reap(ctx, 1); err != nil || count != 1 || agent.closes != 1 || groups.drains != 1 {
			t.Fatal(count, err)
		}
	})
	t.Run("期限同步与失败续约隔离", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		agent := controller.agent.(*fakeAuthority)
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		lease, _ := controller.lease(sessionKeyOf(session))
		if !lease.deadline.Equal(agent.deadline) {
			t.Fatal("代理期限偏离节点期限")
		}
		command.Revision++
		if _, err := controller.Renew(ctx, command); err != nil {
			t.Fatal(err)
		}
		lease, _ = controller.lease(sessionKeyOf(session))
		if !lease.deadline.Equal(agent.deadline) {
			t.Fatal("续约重置传输期限")
		}
		if _, err := controller.Renew(ctx, command); err != nil || agent.renewals != 1 {
			t.Fatal("重复版本再次续约", err)
		}
		agent.failRenew = true
		command.Revision++
		if _, err := controller.Renew(ctx, command); err == nil {
			t.Fatal("失联续约被确认")
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "quarantined" || state.Command.Revision != command.Revision {
			t.Fatal(state, err)
		}
		if _, err = controller.Reap(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("关闭未确认仍清理宿主且不交接", func(t *testing.T) {
		fixture := newFixture(t)
		session := fixture.prepareComputer(t, fixture.specification)
		groups := &fakeGroups{path: t.TempDir()}
		controller := fixture.sessionController(t, groups, sessionOptions())
		agent := controller.agent.(*fakeAuthority)
		command := commandFor(session)
		if _, err := controller.Open(ctx, command); err != nil {
			t.Fatal(err)
		}
		agent.failClose = true
		agent.closedBeforeDrain = func() error {
			if groups.drains != 0 {
				return errors.New("先清理宿主后撤销代理")
			}
			return nil
		}
		if _, err := controller.Close(ctx, command); err == nil || groups.drains != 1 {
			t.Fatal("代理失联跳过宿主清理或返回成功", err)
		}
		state, err := fixture.node.journal.readSession(sessionKeyOf(session))
		if err != nil || state.Phase != "quarantined" {
			t.Fatal(state, err)
		}
		agent.failClose = false
		agent.closedBeforeDrain = nil
		if observation, err := controller.Close(ctx, command); err != nil || !observation.Matches(command, "drained") {
			t.Fatal(observation, err)
		}
	})
}

func TestNodeCloseProvesExitedContainerWithoutGroup(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	agent := controller.agent.(*fakeAuthority)
	agent.failClose = true
	command := commandFor(session)
	if _, err := controller.Close(ctx, command); err == nil {
		t.Fatal("仍在运行的容器没有返回代理确认")
	}
	state, err := fixture.node.journal.readSession(sessionKeyOf(session))
	if err != nil || state == nil || state.Phase != "quarantined" || state.Group != nil {
		t.Fatal("未发出授权的会话没有进入隔离", state, err)
	}
	name := fixture.node.resource(fixture.specification).Name
	fixture.engine.mu.Lock()
	current := fixture.engine.containers[name]
	current.State.Status = "exited"
	current.State.Running = false
	current.State.Pid = 0
	fixture.engine.containers[name] = current
	fixture.engine.mu.Unlock()
	command.Revision++
	observed, err := controller.Close(ctx, command)
	if err != nil || !observed.Matches(command, "drained") || agent.closes != 2 || groups.drains != 0 {
		t.Fatal("核实原容器退出后未能恢复没有宿主组的隔离会话", observed, err)
	}
	state, err = fixture.node.journal.readSession(sessionKeyOf(session))
	if err != nil || state == nil || state.Phase != "closed" || state.Command != command {
		t.Fatal("关闭屏障未持久化", state, err)
	}
	_, err = controller.Open(ctx, command)
	requireFailure(t, err, "session_cannot_reopen")
}

func TestNodeCloseProvesExitedContainer(t *testing.T) {
	cases := []struct {
		name        string
		alter       func(*container)
		drainErr    error
		wantDrained bool
	}{
		{name: "确认原容器退出并清理宿主组", wantDrained: true},
		{name: "确认原容器退出且宿主父组已移除", drainErr: processgroup.ErrContainerGroupAbsent, wantDrained: true},
		{name: "原容器仍有进程", alter: func(current *container) { current.State.Pid = 4242 }},
		{name: "原容器正在重启", alter: func(current *container) { current.State.Status = "restarting" }},
		{name: "容器身份无法确认", alter: func(current *container) { current.Name = "/other-container" }},
		{name: "宿主组清理没有确切证据", drainErr: os.ErrNotExist},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			session := fixture.prepareComputer(t, fixture.specification)
			groups := &fakeGroups{path: t.TempDir()}
			controller := fixture.sessionController(t, groups, sessionOptions())
			agent := controller.agent.(*fakeAuthority)
			command := commandFor(session)
			if _, err := controller.Open(ctx, command); err != nil {
				t.Fatal(err)
			}
			name := fixture.node.resource(fixture.specification).Name
			fixture.engine.mu.Lock()
			original := fixture.engine.containers[name]
			current := original
			current.State.Status = "exited"
			current.State.Running = false
			current.State.Pid = 0
			if test.alter != nil {
				test.alter(&current)
			}
			fixture.engine.containers[name] = current
			fixture.engine.mu.Unlock()
			agent.failClose = true
			groups.drainErr = test.drainErr
			defer func() {
				agent.failClose = false
				groups.drainErr = nil
				fixture.engine.mu.Lock()
				fixture.engine.containers[name] = original
				fixture.engine.mu.Unlock()
			}()
			command.Revision++
			observed, err := controller.Close(ctx, command)
			if test.wantDrained && (err != nil || !observed.Matches(command, "drained")) || !test.wantDrained && (err == nil || observed.State != "") {
				t.Fatal("关闭回执与实际清理证据不一致", observed, err)
			}
			wantPhase := "quarantined"
			if test.wantDrained {
				wantPhase = "closed"
			}
			state, err := fixture.node.journal.readSession(sessionKeyOf(session))
			if err != nil || state == nil || state.Phase != wantPhase || state.Command != command || groups.drains != 1 {
				t.Fatal("持久关闭状态与主机清理证据不一致", state, err)
			}
		})
	}
}
