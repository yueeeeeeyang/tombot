package dockernode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/egressproxy"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type fakeEgress struct {
	ensureCall   func(context.Context, Resource, container) error
	stopCall     func(context.Context, Resource) error
	shutdownCall func(context.Context) error
}

func (egress *fakeEgress) ensure(ctx context.Context, resource Resource, current container) error {
	if egress.ensureCall != nil {
		return egress.ensureCall(ctx, resource, current)
	}
	return nil
}
func (egress *fakeEgress) stop(ctx context.Context, resource Resource) error {
	if egress.stopCall != nil {
		return egress.stopCall(ctx, resource)
	}
	return nil
}
func (egress *fakeEgress) shutdown(ctx context.Context) error {
	if egress.shutdownCall != nil {
		return egress.shutdownCall(ctx)
	}
	return nil
}
func (*fakeEgress) wait(ctx context.Context) error { <-ctx.Done(); return nil }

type fakeProxyInstance struct {
	started    chan struct{}
	fail       chan struct{}
	closed     atomic.Bool
	closeError error
	checkError error
	closeCall  func()
}

func newFakeProxyInstance() *fakeProxyInstance {
	return &fakeProxyInstance{started: make(chan struct{}), fail: make(chan struct{})}
}
func (instance *fakeProxyInstance) serve(ctx context.Context) error {
	close(instance.started)
	select {
	case <-ctx.Done():
		return nil
	case <-instance.fail:
		return errors.New("测试代理异常")
	}
}
func (instance *fakeProxyInstance) close() error {
	if instance.closeCall != nil {
		instance.closeCall()
	}
	instance.closed.Store(true)
	return instance.closeError
}
func (instance *fakeProxyInstance) check() error { return instance.checkError }

func egressFixture(t *testing.T) (fixture, *egressController, Resource, container) {
	t.Helper()
	fixture := newFixture(t)
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	resource := fixture.node.resource(fixture.specification)
	current := fixture.engine.containers[resource.Name]
	current.State.Pid = 42
	fixture.engine.containers[resource.Name] = current
	controller := newEgress(fixture.node, sessionLogger())
	controller.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
		if pid <= 1 {
			return processgroup.ContainerReference{}, errors.New("测试进程无效")
		}
		return processgroup.ContainerReference{ContainerID: id, Path: "docker/" + id, Device: 1, Inode: 2}, ctx.Err()
	}
	controller.clear = func(Resource) error { return nil }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := controller.shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return fixture, controller, resource, current
}

func TestEgressOutlivesRequestAndStopsBeforeContainer(t *testing.T) {
	fixture, controller, resource, current := egressFixture(t)
	instance := newFakeProxyInstance()
	instance.closeCall = func() {
		value, err := fixture.engine.inspectContainer(context.Background(), current.ID)
		if err != nil || !value.State.Running {
			t.Error("关闭出网前容器已被停止或删除", err)
		}
	}
	var creates atomic.Int32
	controller.create = func(context.Context, Resource, processgroup.ContainerReference) (egressInstance, error) {
		creates.Add(1)
		return instance, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := controller.ensure(ctx, resource, current); err != nil {
		t.Fatal(err)
	}
	<-instance.started
	cancel()
	if err := controller.ensure(t.Context(), resource, current); err != nil || creates.Load() != 1 || instance.closed.Load() {
		t.Fatal("开户请求结束撤销端点或重复创建", err)
	}
	fixture.node.egress = controller
	if _, err := fixture.node.Remove(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	if !instance.closed.Load() || len(controller.entries) != 0 {
		t.Fatal("容器清理未先关闭出网端点")
	}
}

func TestEgressFailurePreventsReadyAndRemoval(t *testing.T) {
	fixture := newFixture(t)
	fixture.node.egress = &fakeEgress{ensureCall: func(context.Context, Resource, container) error { return errors.New("测试端点不可用") }}
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err == nil {
		t.Fatal("出网服务失败仍返回电脑就绪")
	}
	state, err := fixture.node.journal.read(fixture.specification.UserID)
	if err != nil || state.Phase == "ready" {
		t.Fatal("提前持久化电脑就绪", err)
	}
	fixture.node.egress = &fakeEgress{stopCall: func(context.Context, Resource) error { return errors.New("测试连接未结束") }}
	before := len(fixture.engine.events)
	if _, err := fixture.node.Remove(t.Context(), fixture.specification); err == nil {
		t.Fatal("出网未关闭仍继续删除")
	}
	if len(fixture.engine.events) != before || fixture.storage.removals != 0 {
		t.Fatal("未知出网清理触发容器删除")
	}
	state, err = fixture.node.journal.read(fixture.specification.UserID)
	if err != nil || state.Phase != "removing" {
		t.Fatal("未先记录删除屏障", err)
	}
}

func TestEgressFailureClosesOtherUsersAndRejectsAdmission(t *testing.T) {
	_, controller, resource, current := egressFixture(t)
	first, second := newFakeProxyInstance(), newFakeProxyInstance()
	controller.create = func(_ context.Context, value Resource, _ processgroup.ContainerReference) (egressInstance, error) {
		if value.Name == resource.Name {
			return first, nil
		}
		return second, nil
	}
	if err := controller.ensure(t.Context(), resource, current); err != nil {
		t.Fatal(err)
	}
	otherSpecification := resource.Specification
	otherSpecification.UserID, otherSpecification.ComputerID, otherSpecification.OperationID = ids.New(), ids.New(), ids.New()
	otherSpecification.VolumeReference = "tombot-user-" + otherSpecification.UserID
	other := controller.node.resource(otherSpecification)
	otherContainer := current
	otherContainer.ID = strings.Repeat("b", 64)
	if err := controller.ensure(t.Context(), other, otherContainer); err != nil {
		t.Fatal(err)
	}
	<-first.started
	<-second.started
	close(first.fail)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := controller.wait(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("出网意外退出未及时报告", err)
	}
	if !first.closed.Load() || !second.closed.Load() {
		t.Fatal("节点异常后仍保留出网服务")
	}
	if err := controller.ensure(t.Context(), resource, current); err == nil {
		t.Fatal("退出后仍接单")
	}
}

func TestEgressBindingCapacityAndCreationFailure(t *testing.T) {
	fixture, controller, resource, current := egressFixture(t)
	instance := newFakeProxyInstance()
	controller.create = func(context.Context, Resource, processgroup.ContainerReference) (egressInstance, error) {
		return instance, nil
	}
	fixture.node.options.MaximumEgressComputers = 1
	if err := controller.ensure(t.Context(), resource, current); err != nil {
		t.Fatal(err)
	}
	changed := current
	changed.ID = strings.Repeat("b", 64)
	if err := controller.ensure(t.Context(), resource, changed); err == nil {
		t.Fatal("旧端点被绑定到另一容器")
	}
	other := resource
	other.Name += "-other"
	if err := controller.ensure(t.Context(), other, current); err == nil {
		t.Fatal("端点数量超出节点上限")
	}
	if err := controller.stop(t.Context(), resource); err != nil {
		t.Fatal(err)
	}
	controller.create = func(context.Context, Resource, processgroup.ContainerReference) (egressInstance, error) {
		return nil, errors.New("测试监听失败")
	}
	if err := controller.ensure(t.Context(), resource, current); err == nil || len(controller.entries) != 0 {
		t.Fatal("失败监听占用端点名额", err)
	}
}

func TestEgressChecksRealContainerBeforePeer(t *testing.T) {
	fixture, controller, resource, current := egressFixture(t)
	parent, err := controller.locate(t.Context(), current.ID, current.State.Pid)
	if err != nil {
		t.Fatal(err)
	}
	peer := egressproxy.Peer{PID: 43}
	if err := controller.checkPeer(t.Context(), resource, parent, peer); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []egressproxy.Peer{{PID: 0}, {PID: 43, UID: 20001}, {PID: 43, GID: 20000}} {
		if err := controller.checkPeer(t.Context(), resource, parent, peer); err == nil {
			t.Fatal("错误内核身份被接纳")
		}
	}
	controller.locate = func(_ context.Context, id string, pid int) (processgroup.ContainerReference, error) {
		altered := parent
		if pid == 43 {
			altered.Inode++
		}
		return altered, nil
	}
	if err := controller.checkPeer(t.Context(), resource, parent, peer); err == nil {
		t.Fatal("其他容器 cgroup 被接纳")
	}
	current.State.Running = false
	fixture.engine.containers[resource.Name] = current
	if err := controller.checkPeer(t.Context(), resource, parent, peer); err == nil {
		t.Fatal("停止的容器仍授权出网")
	}
}

func TestEgressShutdownWaitsForInflightCreation(t *testing.T) {
	_, controller, resource, current := egressFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	instance := newFakeProxyInstance()
	controller.create = func(context.Context, Resource, processgroup.ContainerReference) (egressInstance, error) {
		close(entered)
		<-release
		return instance, nil
	}
	result := make(chan error, 1)
	go func() { result <- controller.ensure(t.Context(), resource, current) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	if err := controller.shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("在途创建被漏掉", err)
	}
	cancel()
	close(release)
	if err := <-result; err == nil {
		t.Fatal("退出期间创建被当作就绪")
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := controller.shutdown(ctx); err != nil || !instance.closed.Load() {
		t.Fatal("退出未等到端点回收", err)
	}
}

func TestEgressRestoreDoesNotReviveLeaseOrCreateContainer(t *testing.T) {
	fixture := newFixture(t)
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.node.journal.read(fixture.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	state.Specification.LeaseExpiresAt = time.Now().Add(-time.Hour)
	if err := fixture.node.journal.save(state); err != nil {
		t.Fatal(err)
	}
	var opened, stopped int
	fixture.node.egress = &fakeEgress{
		ensureCall: func(_ context.Context, resource Resource, _ container) error {
			opened++
			if !resource.Specification.LeaseExpiresAt.Equal(state.Specification.LeaseExpiresAt) {
				t.Fatal("恢复延长了旧租约")
			}
			return nil
		},
		stopCall: func(context.Context, Resource) error { stopped++; return nil },
	}
	before := len(fixture.engine.events)
	if err := fixture.node.restoreEgress(t.Context()); err != nil || opened != 1 || stopped != 0 {
		t.Fatal("运行实例端点未恢复", err)
	}
	state.Phase = "removing"
	if err := fixture.node.journal.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fixture.node.restoreEgress(t.Context()); err != nil || opened != 1 || stopped != 1 {
		t.Fatal("恢复重新开放了删除中的实例", err)
	}
	if len(fixture.engine.events) != before {
		t.Fatal("端点恢复改变了容器生命周期")
	}
	if err := os.WriteFile(filepath.Join(fixture.statePath, "invalid.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.node.restoreEgress(t.Context()); err == nil {
		t.Fatal("恢复忽略了未知状态文件")
	}
}

func TestEgressRestoreFailureClosesRecoveredServices(t *testing.T) {
	fixture := newFixture(t)
	fixture.prepareComputer(t, fixture.specification)
	var stopped atomic.Bool
	fixture.node.egress = &fakeEgress{
		ensureCall:   func(context.Context, Resource, container) error { return errors.New("测试恢复端点失败") },
		shutdownCall: func(context.Context) error { stopped.Store(true); return nil },
	}
	groups := &fakeGroups{path: t.TempDir()}
	if _, err := newSessions(t.Context(), fixture.node, groups, &fakeAuthority{}, sessionOptions(), sessionLogger()); err == nil || !stopped.Load() {
		t.Fatal("恢复失败仍保留已恢复端点", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release, err := fixture.node.journal.lock(ctx, "execution-service")
	if err != nil {
		t.Fatal("失败恢复遗留执行服务锁", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

var _ computer.Runtime = (*Runtime)(nil)
