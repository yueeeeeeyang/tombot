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
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

type memoryBrowserStore struct {
	mu         sync.Mutex
	state      *BrowserSnapshot
	beforeSave func(BrowserSnapshot) error
}

func (store *memoryBrowserStore) LoadBrowser(ctx context.Context) (*BrowserSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state == nil {
		return nil, ctx.Err()
	}
	copy := *store.state
	return &copy, ctx.Err()
}
func (store *memoryBrowserStore) SaveBrowser(ctx context.Context, snapshot BrowserSnapshot) error {
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

// unusedWindowBrowser 不伪造页面能力；本组仅验收监督状态，真实 Chrome 另有集成测试。
type unusedWindowBrowser struct{}

func (unusedWindowBrowser) Open(context.Context, sharedbrowser.Address) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{}, sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) List(context.Context, computer.ExecutionSession) ([]sharedbrowser.Window, error) {
	return nil, sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) Execute(context.Context, sharedbrowser.Address, sharedbrowser.Command) (sharedbrowser.Window, error) {
	return sharedbrowser.Window{}, sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) Screenshot(context.Context, sharedbrowser.Address) ([]byte, error) {
	return nil, sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) RespondDialog(context.Context, sharedbrowser.Address, sharedbrowser.DialogReply) error {
	return sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) CloseWindow(context.Context, sharedbrowser.Address) error {
	return sharedbrowser.ErrUnavailable
}
func (unusedWindowBrowser) ReleaseSession(context.Context, computer.ExecutionSession) error {
	return sharedbrowser.ErrUnavailable
}

type fakeBrowserScope struct {
	files                   []*os.File
	start                   func(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error)
	drain                   func(context.Context) error
	done                    chan struct{}
	doneOnce                sync.Once
	closeOnce               sync.Once
	drained                 atomic.Bool
	revoked                 atomic.Bool
	revokeError, closeError error
}

func (scope *fakeBrowserScope) Start(ctx context.Context, request taskprocess.BrowserRequest, options sharedbrowser.Options) (BrowserService, error) {
	if scope.start != nil {
		return scope.start(ctx, request, options)
	}
	return unusedWindowBrowser{}, ctx.Err()
}
func (scope *fakeBrowserScope) Done() <-chan struct{} { return scope.done }
func (scope *fakeBrowserScope) Revoke() error {
	scope.revoked.Store(true)
	scope.doneOnce.Do(func() { close(scope.done) })
	return scope.revokeError
}
func (scope *fakeBrowserScope) Drain(ctx context.Context) error {
	if scope.drain != nil {
		if err := scope.drain(ctx); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	scope.drained.Store(true)
	scope.doneOnce.Do(func() { close(scope.done) })
	return nil
}
func (scope *fakeBrowserScope) Close() error {
	if !scope.drained.Load() {
		return errors.New("测试浏览器未回收")
	}
	if scope.closeError != nil {
		return scope.closeError
	}
	return scope.release()
}
func (scope *fakeBrowserScope) release() (failure error) {
	scope.closeOnce.Do(func() { failure = agentchannel.CloseFiles(scope.files) })
	return failure
}

type fakeBrowserFactory struct {
	mu      sync.Mutex
	scopes  []*fakeBrowserScope
	prepare func(*fakeBrowserScope)
	err     error
}

func (factory *fakeBrowserFactory) OpenBrowser(_ taskprocess.BrowserRequest, files []*os.File, _ time.Duration) (BrowserScope, error) {
	if factory.err != nil {
		return nil, factory.err
	}
	scope := &fakeBrowserScope{files: files, done: make(chan struct{})}
	if factory.prepare != nil {
		factory.prepare(scope)
	}
	factory.mu.Lock()
	factory.scopes = append(factory.scopes, scope)
	factory.mu.Unlock()
	return scope, nil
}

func browserGrant(fixture fixture) BrowserGrant {
	probe := probeGrant(fixture)
	probe.Request.Group.Kind = processgroup.BrowserService
	return BrowserGrant{Identity: probe.Identity, Revision: 1, Request: taskprocess.BrowserRequest{Version: 1, Group: probe.Request.Group, Deadline: probe.Request.Deadline}, Limits: BrowserLimits{Width: 960, Height: 640, MaximumWindows: 8, MaximumPerSession: 4, MaximumWindowRecords: 32, MaximumEvents: 128, CallTimeoutMS: 1000, DialogTimeoutMS: 10000, ShutdownTimeoutMS: 1000}}
}

func newBrowserControlFixture(t *testing.T, fixture fixture) (*BrowserController, *memoryBrowserStore, *fakeBrowserFactory) {
	t.Helper()
	store, factory := &memoryBrowserStore{}, &fakeBrowserFactory{}
	controller, err := NewBrowserController(t.Context(), fixture.controller.identity, store, factory, fixture.controller.options, func(ctx context.Context, session computer.ExecutionSession) error {
		return fixture.controller.withScope(ctx, session, func(context.Context, *desktopGuard) error { return nil })
	}, fixture.guard.logger, fixture.controller.fail)
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
		select {
		case <-controller.shutdownDone:
		case <-ctx.Done():
			t.Error("测试浏览器协程未结束")
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
func newTestBrowserController(t *testing.T, fixture fixture) *BrowserController {
	controller, _, _ := newBrowserControlFixture(t, fixture)
	return controller
}

func TestBrowserControlLifecycleAndFencing(t *testing.T) {
	fixture := newFixture(t)
	controller, store, factory := newBrowserControlFixture(t, fixture)
	grant := browserGrant(fixture)
	factory.prepare = func(scope *fakeBrowserScope) {
		scope.start = func(ctx context.Context, request taskprocess.BrowserRequest, options sharedbrowser.Options) (BrowserService, error) {
			state, err := store.LoadBrowser(ctx)
			if err != nil || state.Phase != "consumed" || state.Grant.Request != request {
				t.Error("启动前没有持久消费", err)
			}
			if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
				t.Error("浏览器阻塞独立桌面", err)
			}
			if err := options.Authorize(ctx, fixture.command.Session); err != nil {
				t.Error("有效桌面被拒绝", err)
			}
			wrong := fixture.command.Session
			wrong.ContainerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			if options.Authorize(ctx, wrong) == nil {
				t.Error("错误容器获得共享账号访问")
			}
			return unusedWindowBrowser{}, nil
		}
	}
	start := func(grant BrowserGrant) (BrowserObservation, error) {
		return controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second))
	}
	if observed, err := start(grant); err != nil || observed.Phase != "active" {
		t.Fatal(observed, err)
	}
	if _, err := start(grant); err == nil {
		t.Fatal("重复启动未拒绝")
	}
	changed := grant
	changed.Limits.Width--
	if _, err := controller.Close(t.Context(), changed); err == nil {
		t.Fatal("预算改绑关闭了当前实例")
	}
	if observed, err := controller.Status(t.Context(), grant); err != nil || observed.Phase != "active" {
		t.Fatal(observed, err)
	}
	if observed, err := controller.Close(t.Context(), grant); err != nil || observed.Phase != "closed" {
		t.Fatal(observed, err)
	}
	if _, err := controller.Close(t.Context(), grant); err != nil {
		t.Fatal("已确认关闭不幂等", err)
	}
	if len(factory.scopes) != 1 || !factory.scopes[0].drained.Load() {
		t.Fatal("关闭没有确认实际回收")
	}
	next := grant
	next.Revision++
	next.Request.Group.ID = ids.New()
	if _, err := controller.Close(t.Context(), next); err != nil {
		t.Fatal("提前关闭失败", err)
	}
	if _, err := start(next); err == nil {
		t.Fatal("关闭之后允许迟到启动")
	}
	state, err := store.LoadBrowser(t.Context())
	if err != nil || state.Grant != next || state.Phase != "closed" {
		t.Fatal(state, err)
	}
	if _, err := controller.Close(t.Context(), grant); err == nil {
		t.Fatal("旧授权覆盖了较新屏障")
	}
}

func TestBrowserControlConcurrentStartup(t *testing.T) {
	fixture := newFixture(t)
	controller, _, factory := newBrowserControlFixture(t, fixture)
	grant := browserGrant(fixture)
	var accepted atomic.Int32
	var pending sync.WaitGroup
	for range 12 {
		pending.Go(func() {
			if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err == nil {
				accepted.Add(1)
			}
		})
	}
	pending.Wait()
	if accepted.Load() != 1 || len(factory.scopes) != 1 {
		t.Fatal("并发启动消费了多次授权", accepted.Load(), len(factory.scopes))
	}
}

func TestBrowserControlFailureAndExit(t *testing.T) {
	for _, phase := range []string{"接管", "消费保存", "启动", "活动保存", "回收", "关闭入口", "关闭保存", "异常退出"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newFixture(t)
			controller, store, factory := newBrowserControlFixture(t, fixture)
			grant := browserGrant(fixture)
			failure := errors.New("测试浏览器故障")
			factory.prepare = func(scope *fakeBrowserScope) {
				switch phase {
				case "启动":
					scope.start = func(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error) {
						return nil, failure
					}
				case "回收":
					scope.drain = func(context.Context) error { return failure }
				case "关闭入口":
					scope.closeError = failure
				}
			}
			if phase == "接管" {
				factory.err = failure
			}
			store.beforeSave = func(snapshot BrowserSnapshot) error {
				if phase == "消费保存" && snapshot.Phase == "consumed" || phase == "活动保存" && snapshot.Phase == "active" || phase == "关闭保存" && snapshot.Phase == "closed" {
					return failure
				}
				return nil
			}
			observed, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second))
			if phase == "接管" || phase == "消费保存" || phase == "启动" || phase == "活动保存" {
				if err == nil || observed != (BrowserObservation{}) {
					t.Fatal("启动失败被报告成功", observed, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if phase == "异常退出" {
				factory.scopes[0].doneOnce.Do(func() { close(factory.scopes[0].done) })
				select {
				case <-controller.fatal:
				case <-time.After(time.Second):
					t.Fatal("浏览器退出未撤销容器能力")
				}
				return
			}
			if observed, err := controller.Close(t.Context(), grant); err == nil || observed != (BrowserObservation{}) {
				t.Fatal("关闭失败被报告成功", observed, err)
			}
			select {
			case <-controller.fatal:
			default:
				t.Fatal("无法回收仍允许接单")
			}
		})
	}
}

func TestBrowserControlCancellationAndBlockedPersistence(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "启动取消", true: "持久化阻塞"}[blocked], func(t *testing.T) {
			fixture := newFixture(t)
			controller, store, factory := newBrowserControlFixture(t, fixture)
			grant := browserGrant(fixture)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			if blocked {
				store.beforeSave = func(snapshot BrowserSnapshot) error {
					if snapshot.Phase == "consumed" {
						close(entered)
						<-release
					}
					return nil
				}
			} else {
				factory.prepare = func(scope *fakeBrowserScope) {
					scope.start = func(ctx context.Context, _ taskprocess.BrowserRequest, _ sharedbrowser.Options) (BrowserService, error) {
						close(entered)
						<-ctx.Done()
						return nil, ctx.Err()
					}
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := controller.startAt(ctx, grant, probeFiles(t), time.Now().Add(3*time.Second))
				finished <- err
			}()
			<-entered
			cancel()
			deadline := time.After(time.Second)
			for !factory.scopes[0].revoked.Load() {
				select {
				case <-deadline:
					t.Fatal("取消未独立撤销启动")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("取消返回成功")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("取消被阻塞保存拖住")
			}
			if blocked {
				select {
				case <-controller.fatal:
				default:
					t.Fatal("保存阻塞没有通知退出")
				}
			}
			once.Do(func() { close(release) })
		})
	}
}

func TestBrowserControlRestartBoundary(t *testing.T) {
	for _, phase := range []string{"consumed", "active", "closed"} {
		for _, generation := range []int64{1, 2} {
			t.Run(phase+string(rune('0'+generation)), func(t *testing.T) {
				fixture := newFixture(t)
				grant := browserGrant(fixture)
				identity := grant.Identity
				identity.Generation = generation
				store := &memoryBrowserStore{state: &BrowserSnapshot{Version: 1, Grant: grant, Phase: phase}}
				controller, err := NewBrowserController(t.Context(), identity, store, &fakeBrowserFactory{}, fixture.controller.options, func(context.Context, computer.ExecutionSession) error { return nil }, fixture.guard.logger, fixture.controller.fail)
				if generation == 1 && phase != "closed" {
					if err == nil {
						t.Fatal("同代次复活未关闭服务")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := controller.Shutdown(t.Context()); err != nil {
						t.Error(err)
					}
				}()
				state, err := store.LoadBrowser(t.Context())
				if err != nil || state.Phase != "closed" || state.Grant.Revision != grant.Revision {
					t.Fatal("恢复丢失版本屏障", state, err)
				}
				grant.Identity = identity
				if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err == nil {
					t.Fatal("跨代次复用旧授权")
				}
			})
		}
	}
}

func TestBrowserControlRejectsIdentityAndBudgetChanges(t *testing.T) {
	fixture := newFixture(t)
	controller, _, factory := newBrowserControlFixture(t, fixture)
	grant := browserGrant(fixture)
	for _, change := range []func(*BrowserGrant){
		func(g *BrowserGrant) { g.Identity.UserID = ids.New() },
		func(g *BrowserGrant) { g.Identity.ComputerID = ids.New() },
		func(g *BrowserGrant) { g.Identity.NodeID = ids.New() },
		func(g *BrowserGrant) { g.Identity.Generation++ },
		func(g *BrowserGrant) { g.Revision = 0 },
		func(g *BrowserGrant) { g.Request.Group.Kind = processgroup.ProbeService },
		func(g *BrowserGrant) { g.Limits.CallTimeoutMS = int(^uint(0) >> 1) },
		func(g *BrowserGrant) { g.Limits.DialogTimeoutMS = int(^uint(0) >> 1) },
		func(g *BrowserGrant) { g.Limits.DialogTimeoutMS = 0 },
		func(g *BrowserGrant) { g.Limits.CallTimeoutMS = 2000 },
		func(g *BrowserGrant) { g.Limits.ShutdownTimeoutMS = 2000 },
		func(g *BrowserGrant) { g.Limits.MaximumPerSession = g.Limits.MaximumWindows + 1 },
	} {
		changed := grant
		change(&changed)
		files := probeFiles(t)
		if observed, err := controller.startAt(t.Context(), changed, files, time.Now().Add(time.Second)); err == nil || observed != (BrowserObservation{}) {
			t.Fatal("非法授权通过", observed, err)
		}
		for _, file := range files {
			if _, err := file.Stat(); err == nil {
				t.Fatal("拒绝请求遗留传入描述符")
			}
		}
	}
	if len(factory.scopes) != 0 {
		t.Fatal("非法请求触发资源接管")
	}
	for _, change := range []func(*BrowserSnapshot){
		func(s *BrowserSnapshot) { s.Version++ },
		func(s *BrowserSnapshot) { s.Phase = "ready" },
		func(s *BrowserSnapshot) { s.Grant.Identity.UserID = ids.New() },
		func(s *BrowserSnapshot) { s.Grant.Identity.Generation++ },
	} {
		state := BrowserSnapshot{Version: 1, Grant: grant, Phase: "closed"}
		change(&state)
		if restored, err := NewBrowserController(t.Context(), grant.Identity, &memoryBrowserStore{state: &state}, factory, fixture.controller.options, func(context.Context, computer.ExecutionSession) error { return nil }, fixture.guard.logger, fixture.controller.fail); err == nil || restored != nil {
			t.Fatal("无效恢复记录被接受", err)
		}
	}
}

func TestBrowserControlShutdownPersistsClosure(t *testing.T) {
	fixture := newFixture(t)
	controller, store, factory := newBrowserControlFixture(t, fixture)
	grant := browserGrant(fixture)
	if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var pending sync.WaitGroup
	for range 8 {
		pending.Go(func() {
			if err := controller.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	pending.Wait()
	state, err := store.LoadBrowser(t.Context())
	if err != nil || state.Phase != "closed" || !factory.scopes[0].drained.Load() {
		t.Fatal("停服未持久关闭", state, err)
	}
	if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err == nil {
		t.Fatal("停服后继续接单")
	}
}
