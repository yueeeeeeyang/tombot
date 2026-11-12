package computeragent

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

type memoryProbeStore struct {
	mu         sync.Mutex
	state      *ProbeSnapshot
	beforeSave func(ProbeSnapshot) error
}

func (store *memoryProbeStore) LoadProbe(ctx context.Context) (*ProbeSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state == nil {
		return nil, ctx.Err()
	}
	copy := *store.state
	return &copy, ctx.Err()
}

func (store *memoryProbeStore) SaveProbe(ctx context.Context, snapshot ProbeSnapshot) error {
	if store.beforeSave != nil {
		if err := store.beforeSave(snapshot); err != nil {
			return err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.state = &snapshot
	return ctx.Err()
}

type fakeProbeScope struct {
	files                               []*os.File
	run                                 func(context.Context, taskprocess.ProbeRequest, *os.File) (computerprobe.Evidence, error)
	revoked                             chan struct{}
	revokeOnce                          sync.Once
	closeOnce                           sync.Once
	revokeError, drainError, closeError error
	drained                             atomic.Bool
}

func (scope *fakeProbeScope) Run(ctx context.Context, request taskprocess.ProbeRequest, revocation *os.File) (computerprobe.Evidence, error) {
	if scope.run != nil {
		return scope.run(ctx, request, revocation)
	}
	return probeEvidence(request.Group.ID), nil
}
func (scope *fakeProbeScope) Revoke() error {
	scope.revokeOnce.Do(func() { close(scope.revoked) })
	return scope.revokeError
}
func (scope *fakeProbeScope) Drain(ctx context.Context) error {
	if scope.drainError != nil {
		return scope.drainError
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	scope.drained.Store(true)
	return nil
}
func (scope *fakeProbeScope) Close() error {
	if !scope.drained.Load() {
		return errors.New("测试探针尚未回收")
	}
	if scope.closeError != nil {
		return scope.closeError
	}
	return scope.release()
}
func (scope *fakeProbeScope) release() (failure error) {
	scope.closeOnce.Do(func() { failure = agentchannel.CloseFiles(scope.files) })
	return failure
}

type fakeProbeFactory struct {
	mu      sync.Mutex
	scopes  []*fakeProbeScope
	prepare func(*fakeProbeScope)
	err     error
}

func (factory *fakeProbeFactory) OpenProbe(_ processgroup.ServiceReference, files []*os.File) (ProbeScope, error) {
	if factory.err != nil {
		return nil, factory.err
	}
	scope := &fakeProbeScope{files: files, revoked: make(chan struct{})}
	if factory.prepare != nil {
		factory.prepare(scope)
	}
	factory.mu.Lock()
	factory.scopes = append(factory.scopes, scope)
	factory.mu.Unlock()
	return scope, nil
}

func probeEvidence(id string) computerprobe.Evidence {
	return computerprobe.Evidence{ServiceID: id, Terminal: true, Files: true, LocalNetwork: true, FilesystemType: 0xef53, FilesystemSize: 32 << 20}
}

func probeGrant(fixture fixture) ProbeGrant {
	return ProbeGrant{Identity: fixture.controller.identity, Revision: 1, Request: taskprocess.ProbeRequest{Version: 1, Group: processgroup.ServiceReference{Container: processgroup.ContainerReference{ContainerID: fixture.command.Session.ContainerID, Path: fixture.reference.ParentPath, Device: fixture.reference.ParentDevice, Inode: fixture.reference.ParentInode}, ID: ids.New(), Kind: processgroup.ProbeService}, Deadline: agentchannel.Deadline{ExpiresNS: 1, TimeNamespace: 1}}}
}

func probeFiles(t *testing.T) []*os.File {
	t.Helper()
	var files []*os.File
	for range 3 {
		file, err := os.CreateTemp(t.TempDir(), "probe-control")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}

func newProbeFixture(t *testing.T, fixture fixture) (*ProbeController, *memoryProbeStore, *fakeProbeFactory) {
	t.Helper()
	store, factory := &memoryProbeStore{}, &fakeProbeFactory{}
	controller, err := NewProbeController(t.Context(), fixture.controller.identity, store, factory, fixture.controller.options, fixture.guard.logger, fixture.controller.fail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := controller.Shutdown(ctx)
		select {
		case <-controller.fatal:
		default:
			if err != nil {
				t.Error(err)
			}
		}
		// 故障测试的入口退出由测试替身表示；等待在途协程再释放模拟描述符，避免掩盖竞态。
		done := make(chan struct{})
		go func() { controller.pending.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("测试探针协程未结束")
			return
		}
		for _, scope := range factory.scopes {
			if err := scope.release(); err != nil {
				t.Error(err)
			}
		}
	})
	return controller, store, factory
}

func newTestProbeController(t *testing.T, fixture fixture) *ProbeController {
	controller, _, _ := newProbeFixture(t, fixture)
	return controller
}

func TestProbeConsumptionAndIndependentExecution(t *testing.T) {
	fixture := newFixture(t)
	controller, store, factory := newProbeFixture(t, fixture)
	grant := probeGrant(fixture)
	var runs atomic.Int32
	factory.prepare = func(scope *fakeProbeScope) {
		scope.run = func(ctx context.Context, request taskprocess.ProbeRequest, _ *os.File) (computerprobe.Evidence, error) {
			runs.Add(1)
			state, err := store.LoadProbe(ctx)
			if err != nil || state.Phase != "consumed" || state.Grant.Request != request {
				t.Error("执行前未持久消费授权", err)
			}
			if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
				t.Error("探针阻塞了独立 Bot 会话", err)
			}
			return probeEvidence(request.Group.ID), nil
		}
	}
	check := func(grant ProbeGrant) (computerprobe.Evidence, error) {
		return controller.checkAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second))
	}
	evidence, err := check(grant)
	if err != nil || evidence.Validate(grant.Request.Group.ID) != nil {
		t.Fatal("完整探针执行失败", evidence, err)
	}
	state, err := store.LoadProbe(t.Context())
	if err != nil || state.Phase != "closed" || !factory.scopes[0].drained.Load() {
		t.Fatal("返回证据前未回收及持久关闭", err)
	}
	for _, rejected := range []ProbeGrant{grant, {Identity: grant.Identity, Revision: 2, Request: grant.Request}} {
		if proof, err := check(rejected); err == nil || proof != (computerprobe.Evidence{}) {
			t.Fatal("重复授权或服务实例被再次执行")
		}
	}
	grant.Revision++
	grant.Request.Group.ID = ids.New()
	if _, err := check(grant); err != nil || runs.Load() != 2 {
		t.Fatal("新版本没有独立执行", err)
	}
}

func TestProbeFailureNeverReturnsEvidence(t *testing.T) {
	for _, stage := range []string{"接管", "消费持久化", "执行", "证据", "终止", "回收", "关闭入口", "关闭持久化"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newFixture(t)
			controller, store, factory := newProbeFixture(t, fixture)
			failure := errors.New("测试探针故障")
			if stage == "接管" {
				factory.err = failure
			}
			store.beforeSave = func(snapshot ProbeSnapshot) error {
				if stage == "消费持久化" && snapshot.Phase == "consumed" || stage == "关闭持久化" && snapshot.Phase == "closed" {
					return failure
				}
				return nil
			}
			factory.prepare = func(scope *fakeProbeScope) {
				switch stage {
				case "执行":
					scope.run = func(context.Context, taskprocess.ProbeRequest, *os.File) (computerprobe.Evidence, error) {
						return computerprobe.Evidence{}, failure
					}
				case "证据":
					scope.run = func(context.Context, taskprocess.ProbeRequest, *os.File) (computerprobe.Evidence, error) {
						return probeEvidence(ids.New()), nil
					}
				case "终止":
					scope.revokeError = failure
				case "回收":
					scope.drainError = failure
				case "关闭入口":
					scope.closeError = failure
				}
			}
			proof, err := controller.checkAt(t.Context(), probeGrant(fixture), probeFiles(t), time.Now().Add(time.Second))
			if err == nil || proof != (computerprobe.Evidence{}) {
				t.Fatal("失败返回了成功证据", proof, err)
			}
			wantFatal := stage != "接管" && stage != "执行" && stage != "证据"
			fatal := false
			select {
			case <-controller.fatal:
				fatal = true
			default:
			}
			if fatal != wantFatal {
				t.Fatal("未知持久化或回收结果未触发预期退出", fatal)
			}
		})
	}
}

func TestConcurrentProbeRequestIsConsumedOnce(t *testing.T) {
	fixture := newFixture(t)
	controller, _, factory := newProbeFixture(t, fixture)
	grant := probeGrant(fixture)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	factory.prepare = func(scope *fakeProbeScope) {
		scope.run = func(ctx context.Context, request taskprocess.ProbeRequest, _ *os.File) (computerprobe.Evidence, error) {
			close(entered)
			select {
			case <-release:
				return probeEvidence(request.Group.ID), nil
			case <-ctx.Done():
				return computerprobe.Evidence{}, ctx.Err()
			}
		}
	}
	results := make(chan error, 12)
	for range 12 {
		files := probeFiles(t)
		go func() {
			_, err := controller.checkAt(t.Context(), grant, files, time.Now().Add(time.Second))
			results <- err
		}()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("并发探针未开始")
	}
	once.Do(func() { close(release) })
	succeeded := 0
	for range 12 {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 || len(factory.scopes) != 1 {
		t.Fatal("并发重复探针执行多次", succeeded, len(factory.scopes))
	}
}

func TestProbeShutdownRevokesInFlightExecution(t *testing.T) {
	fixture := newFixture(t)
	controller, _, factory := newProbeFixture(t, fixture)
	started := make(chan *fakeProbeScope, 1)
	factory.prepare = func(scope *fakeProbeScope) {
		scope.run = func(ctx context.Context, _ taskprocess.ProbeRequest, _ *os.File) (computerprobe.Evidence, error) {
			started <- scope
			<-ctx.Done()
			return computerprobe.Evidence{}, ctx.Err()
		}
	}
	result := make(chan error, 1)
	files := probeFiles(t)
	go func() {
		_, err := controller.checkAt(t.Context(), probeGrant(fixture), files, time.Now().Add(time.Second))
		result <- err
	}()
	scope := <-started
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scope.revoked:
	default:
		t.Fatal("停服没有撤销探针")
	}
	if err := <-result; err == nil {
		t.Fatal("停服取消被当作检查成功")
	}
	if _, err := controller.checkAt(t.Context(), probeGrant(fixture), probeFiles(t), time.Now().Add(time.Second)); err == nil {
		t.Fatal("停服后仍然接单")
	}
}

func TestProbeBlockedPersistenceCannotDisableRevocation(t *testing.T) {
	fixture := newFixture(t)
	controller, store, factory := newProbeFixture(t, fixture)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	store.beforeSave = func(snapshot ProbeSnapshot) error {
		if snapshot.Phase == "consumed" {
			close(entered)
			<-release
		}
		return nil
	}
	started := make(chan *fakeProbeScope, 1)
	factory.prepare = func(scope *fakeProbeScope) { started <- scope }
	files := probeFiles(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := controller.checkAt(ctx, probeGrant(fixture), files, time.Now().Add(time.Second))
		result <- err
	}()
	<-entered
	scope := <-started
	cancel()
	select {
	case <-scope.revoked:
	case <-time.After(time.Second):
		t.Fatal("刷盘阻塞期间未独立撤销")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("阻塞回收没有失败")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("阻塞持久化禁用了退出上限")
	}
	select {
	case <-fixture.controller.fatal:
	default:
		t.Fatal("未通知实际容器监督器退出")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestProbeRestartAndRevisionBoundaries(t *testing.T) {
	fixture := newFixture(t)
	grant := probeGrant(fixture)
	for _, phase := range []string{"consumed", "closed"} {
		store := &memoryProbeStore{state: &ProbeSnapshot{Version: 1, Grant: grant, Phase: phase}}
		controller, err := NewProbeController(t.Context(), grant.Identity, store, &fakeProbeFactory{}, fixture.controller.options, fixture.guard.logger, fixture.controller.fail)
		if (err == nil) != (phase == "closed") {
			t.Fatal("同代次旧探针恢复条件错误", phase, err)
		}
		if controller != nil {
			if err := controller.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		newIdentity := grant.Identity
		newIdentity.Generation++
		controller, err = NewProbeController(t.Context(), newIdentity, store, &fakeProbeFactory{}, fixture.controller.options, fixture.guard.logger, fixture.controller.fail)
		if err != nil {
			t.Fatal("新容器代次未封闭旧探针", err)
		}
		if controller.state.Phase != "closed" || controller.state.Grant.Revision != grant.Revision {
			t.Fatal("恢复丢失消费版本")
		}
		if err := controller.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
