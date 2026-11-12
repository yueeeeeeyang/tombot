package dockernode

import (
	"context"
	"errors"
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
