package computeragent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type memoryStore struct {
	mu                  sync.Mutex
	state               *Snapshot
	failPhase           string
	failCount           int
	failLoadCount       int
	commitBeforeFailure bool
	blockClosing        <-chan struct{}
	closingStarted      chan<- struct{}
}

func (store *memoryStore) Load(ctx context.Context) (*Snapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if store.failLoadCount > 0 {
		store.failLoadCount--
		return nil, errors.New("测试桌面记录读取失败")
	}
	if store.state == nil {
		return nil, nil
	}
	copy := *store.state
	return &copy, nil
}
func (store *memoryStore) Save(ctx context.Context, value Snapshot) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if value.Phase == "closing" && store.blockClosing != nil {
		select {
		case store.closingStarted <- struct{}{}:
		default:
		}
		<-store.blockClosing
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if store.failCount > 0 && value.Phase == store.failPhase {
		store.failCount--
		if store.commitBeforeFailure {
			store.state = &value
		}
		return errors.New("测试记录保存结果未知")
	}
	store.state = &value
	return nil
}

type fakeScope struct {
	revoked                atomic.Bool
	failRevoke             atomic.Bool
	files                  []*os.File
	path                   string
	terminations, failures int
	closed                 bool
	beforeTerminate        func() error
}

func (scope *fakeScope) Revoke() error {
	scope.revoked.Store(true)
	if scope.failRevoke.Load() {
		return errors.New("测试独立终止失败")
	}
	return nil
}

func (scope *fakeScope) Membership(role string) (*os.File, error) {
	if scope.closed || role != "shell" && role != "browser" {
		return nil, errors.New("测试资源组已关闭或角色错误")
	}
	return os.Open(scope.path)
}
func (scope *fakeScope) Terminate(ctx context.Context) error {
	scope.terminations++
	if scope.beforeTerminate != nil {
		if err := scope.beforeTerminate(); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scope.failures > 0 {
		scope.failures--
		return errors.New("测试资源组仍有活进程")
	}
	return nil
}
func (scope *fakeScope) Close() error {
	if scope.closed {
		return nil
	}
	scope.closed = true
	return agentchannel.CloseFiles(scope.files)
}

type fakeScopes struct {
	mu     sync.Mutex
	path   string
	scopes []*fakeScope
}

func (factory *fakeScopes) Open(_ processgroup.Reference, files []*os.File) (Scope, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	scope := &fakeScope{files: files, path: factory.path}
	factory.scopes = append(factory.scopes, scope)
	return scope, nil
}

type fixture struct {
	controller *Controller
	guard      *desktopGuard
	store      *memoryStore
	factory    *fakeScopes
	command    computer.SessionCommand
	reference  processgroup.Reference
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	session := computer.ExecutionSession{DesktopIdentity: computer.DesktopIdentity{UserID: ids.New(), ComputerID: ids.New(), BotID: ids.New(), DesktopID: ids.New(), UID: computer.DesktopUIDBase + 1, GID: computer.DesktopSharedGID, DisplayNumber: 1}, NodeID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1, SessionID: ids.New(), RunID: ids.New(), FencingToken: 1}
	command := computer.SessionCommand{Session: session, Revision: 1, Lifetime: 5 * time.Second}
	reference := processgroup.Reference{Session: session, ParentPath: "system.slice/docker-" + session.ContainerID + ".scope", ParentDevice: 1, ParentInode: 2}
	file, err := os.CreateTemp(t.TempDir(), "group")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{}
	factory := &fakeScopes{path: file.Name()}
	catalog := &memoryCatalog{records: map[string]*memoryStore{session.DesktopID: store}}
	controller, err := NewController(context.Background(), Identity{session.UserID, session.ComputerID, session.NodeID, session.Generation}, catalog, factory, Options{CallTimeout: time.Second, CleanupTimeout: time.Second, RetryDelay: 10 * time.Millisecond, CleanupAttempts: 2}, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	guard, err := controller.desktop(context.Background(), session.DesktopIdentity, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.mu.Lock()
		store.failCount = 0
		store.mu.Unlock()
		for _, scope := range factory.scopes {
			scope.failRevoke.Store(false)
			scope.failures = 0
			scope.beforeTerminate = nil
		}
		if err := controller.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return fixture{controller, guard, store, factory, command, reference}
}
func (fixture fixture) files(t *testing.T) []*os.File {
	t.Helper()
	var files []*os.File
	for range 4 {
		file, err := os.Open(fixture.factory.path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	return files
}
func (fixture fixture) open(t *testing.T) {
	t.Helper()
	if observation, err := fixture.guard.Open(context.Background(), fixture.command, fixture.reference, fixture.files(t)); err != nil || !observation.Matches(fixture.command, "active") {
		t.Fatalf("开启代理会话失败：%v", err)
	}
}

func TestSessionAuthorityAndFences(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	duplicate := fixture.files(t)
	deadline := fixture.guard.permit.deadline
	if _, err := fixture.guard.Open(context.Background(), fixture.command, fixture.reference, duplicate); err != nil {
		t.Fatal(err)
	}
	for _, file := range duplicate {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("重复开启未关闭收到的描述符", err)
		}
	}
	if !fixture.guard.permit.deadline.Equal(deadline) || len(fixture.factory.scopes) != 1 {
		t.Fatal("重复开启延长授权或重新接管资源组")
	}
	if _, err := fixture.guard.Renew(context.Background(), fixture.command); err != nil {
		t.Fatal(err)
	}
	if !fixture.guard.permit.deadline.Equal(deadline) {
		t.Fatal("重复续约延长了期限")
	}
	called := false
	if err := fixture.guard.WithTask(context.Background(), fixture.command.Session, "shell", func(_ context.Context, reference processgroup.Reference, membership, revocation *os.File) error {
		called = true
		if reference != fixture.reference || membership == nil || revocation == nil {
			return errors.New("启动授权身份错误")
		}
		return nil
	}); err != nil || !called {
		t.Fatal("正常任务未取得受限授权", err)
	}
	wrong := fixture.command.Session
	wrong.UserID = ids.New()
	if err := fixture.guard.WithTask(context.Background(), wrong, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error {
		t.Error("跨用户启动被执行")
		return nil
	}); err == nil {
		t.Fatal("跨用户授权被接受")
	}
	closeCommand := fixture.command
	closeCommand.Revision++
	if observation, err := fixture.guard.Close(context.Background(), closeCommand); err != nil || !observation.Matches(closeCommand, "closed") {
		t.Fatal("关闭失败", err)
	}
	if fixture.factory.scopes[0].terminations != 1 || !fixture.factory.scopes[0].closed {
		t.Fatal("关闭没有完整回收资源组")
	}
	if _, err := fixture.guard.Open(context.Background(), fixture.command, fixture.reference, fixture.files(t)); err == nil {
		t.Fatal("已关闭会话被复活")
	}
	if _, err := fixture.guard.Renew(context.Background(), closeCommand); err == nil {
		t.Fatal("关闭后仍可续约")
	}
	next := fixture.command
	next.Session.SessionID, next.Session.RunID = ids.New(), ids.New()
	next.Session.FencingToken++
	nextReference := fixture.reference
	nextReference.Session = next.Session
	if _, err := fixture.guard.Open(context.Background(), next, nextReference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.guard.Close(context.Background(), closeCommand); err == nil {
		t.Fatal("旧关闭影响新会话")
	}
	if !fixture.guard.permit.active(next.Revision) || fixture.factory.scopes[1].terminations != 0 {
		t.Fatal("新会话被旧命令清理")
	}
}

func TestExpiryRevokesWhileTaskStartHoldsGate(t *testing.T) {
	fixture := newFixture(t)
	deadline := time.Now().Add(150 * time.Millisecond)
	if _, err := fixture.guard.openAt(context.Background(), fixture.command, fixture.reference, fixture.files(t), deadline); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- fixture.guard.WithTask(context.Background(), fixture.command.Session, "shell", func(_ context.Context, _ processgroup.Reference, _ *os.File, revocation *os.File) error {
			if err := revocation.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				return err
			}
			close(started)
			var one [1]byte
			if count, err := revocation.Read(one[:]); count != 0 || !errors.Is(err, io.EOF) {
				return errors.New("阻塞启动期间没有先撤销授权")
			}
			return nil
		})
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatal("测试启动回调未进入受管区间", err)
	case <-time.After(time.Second):
		t.Fatal("测试启动回调未开始")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("过期启动被报告成功")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("撤销计时器被业务锁阻塞")
	}
	if _, err := fixture.guard.Renew(context.Background(), computer.SessionCommand{Session: fixture.command.Session, Revision: 2, Lifetime: time.Second}); err == nil {
		t.Fatal("已撤销管道被续约复活")
	}
	if _, err := fixture.guard.Close(context.Background(), fixture.command); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownStoreCommitCannotLowerRevision(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	fixture.store.mu.Lock()
	fixture.store.failPhase, fixture.store.failCount, fixture.store.commitBeforeFailure = "active", 1, true
	fixture.store.mu.Unlock()
	renewed := fixture.command
	renewed.Revision++
	if _, err := fixture.guard.Renew(context.Background(), renewed); err == nil {
		t.Fatal("未知保存结果被报告成功")
	}
	if _, err := fixture.guard.Close(context.Background(), fixture.command); err == nil {
		t.Fatal("旧关闭降低了未知已提交版本")
	}
	if err := fixture.guard.WithTask(context.Background(), renewed.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error {
		t.Error("未知续约后执行用户启动")
		return nil
	}); err == nil {
		t.Fatal("保存失败没有撤销授权")
	}
	if _, err := fixture.guard.Close(context.Background(), renewed); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.Load(context.Background())
	if err != nil || snapshot.Command.Revision != renewed.Revision || snapshot.Phase != "closed" {
		t.Fatal("关闭记录未保留最高版本", err)
	}
}

func TestCleanupRetryAndClosedPersistence(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	scope := fixture.factory.scopes[0]
	scope.beforeTerminate = func() error {
		if err := fixture.guard.permit.reader.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			return err
		}
		var one [1]byte
		if _, err := fixture.guard.permit.reader.Read(one[:]); !errors.Is(err, io.EOF) {
			return errors.New("资源组清理前未撤销引导")
		}
		return nil
	}
	scope.failures = 1
	if _, err := fixture.guard.Close(context.Background(), fixture.command); err == nil {
		t.Fatal("清理失败被报告关闭")
	}
	if fixture.guard.state.Phase != "closing" || fixture.guard.permit.active(fixture.command.Revision) {
		t.Fatal("清理失败仍允许启动")
	}
	fixture.store.mu.Lock()
	fixture.store.failPhase, fixture.store.failCount = "closed", 1
	fixture.store.mu.Unlock()
	if _, err := fixture.guard.Close(context.Background(), fixture.command); err == nil {
		t.Fatal("关闭记录失败被报告成功")
	}
	scope.beforeTerminate = nil
	if _, err := fixture.guard.Close(context.Background(), fixture.command); err != nil {
		t.Fatal("物理清理后不能重试最终记录", err)
	}
}

func TestRestartRequiresInstanceIsolation(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	identity := fixture.guard.identity
	if _, err := newGuard(context.Background(), identity, fixture.command.Session.DesktopIdentity, fixture.guard.capacity, fixture.store, fixture.factory, fixture.guard.options, fixture.guard.logger); err == nil {
		t.Fatal("同一容器重启恢复了旧租约")
	}
	identity.Generation++
	recovered, err := newGuard(context.Background(), identity, fixture.command.Session.DesktopIdentity, fixture.guard.capacity, fixture.store, fixture.factory, fixture.guard.options, fixture.guard.logger)
	if err != nil || recovered.state.Phase != "closed" || recovered.permit != nil {
		t.Fatal("新实例未封闭旧会话", err)
	}
	identity.UserID = ids.New()
	if _, err := newGuard(context.Background(), identity, fixture.command.Session.DesktopIdentity, fixture.guard.capacity, fixture.store, fixture.factory, fixture.guard.options, fixture.guard.logger); err == nil {
		t.Fatal("另一用户接管了持久记录")
	}
}

func TestWatchdogCleanupAttemptsAreBounded(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	fixture.factory.scopes[0].failures = 10
	if err := fixture.guard.permit.revoke(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fixture.guard.Serve(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("看门狗没有在有限清理失败后要求退出", err)
	}
	if fixture.factory.scopes[0].terminations != fixture.guard.options.CleanupAttempts {
		t.Fatal("清理次数无界或提前放行")
	}
}

func TestWatchdogSignalsWhileStoreIsBlocked(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	started := make(chan struct{}, 1)
	fixture.store.mu.Lock()
	fixture.store.blockClosing, fixture.store.closingStarted = release, started
	fixture.store.mu.Unlock()
	if err := fixture.guard.permit.revoke(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.guard.Serve(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("测试未进入阻塞的关闭记录保存")
	}
	if !fixture.factory.scopes[0].revoked.Load() {
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for !fixture.factory.scopes[0].revoked.Load() {
			select {
			case <-deadline.C:
				t.Fatal("独立终止信号等待了磁盘保存")
			case <-ticker.C:
			}
		}
	}
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("阻塞回收未触发自身致命期限", err)
		}
	case <-ctx.Done():
		t.Fatal("看门狗被本地文件系统阻塞")
	}
	if err := fixture.guard.WithTask(context.Background(), fixture.command.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error {
		t.Error("致命故障后启动了任务")
		return nil
	}); err == nil {
		t.Fatal("致命故障后仍接收任务")
	}
	once.Do(func() { close(release) })
	// 等待被阻塞的工作退出临界区后，测试才能释放其受控替身；正式入口遇到致命故障应直接退出。
	_, leave, err := fixture.guard.acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	leave()
}

func TestIndependentSignalFailureIsFatal(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	fixture.factory.scopes[0].failRevoke.Store(true)
	if err := fixture.guard.permit.revoke(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fixture.guard.Serve(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("独立终止失败没有要求退出", err)
	}
	// 正式入口此时直接退出；测试进程仍继续运行，须等已进入的回收工作退出后再重置替身。
	_, leave, err := fixture.guard.acquire(context.Background(), true)
	if err != nil {
		t.Fatal("致命故障后的测试回收未退出临界区", err)
	}
	leave()
}

func TestUnknownCloseCommitKeepsHighestFence(t *testing.T) {
	fixture := newFixture(t)
	closed := fixture.command
	closed.Session.FencingToken = 10
	fixture.store.failPhase, fixture.store.failCount, fixture.store.commitBeforeFailure = "closed", 1, true
	if _, err := fixture.guard.Close(context.Background(), closed); err == nil {
		t.Fatal("未知关闭提交被报告成功")
	}
	if _, err := fixture.guard.Open(context.Background(), fixture.command, fixture.reference, fixture.files(t)); err == nil {
		t.Fatal("未知关闭结果允许旧控制者开启")
	}
	if _, err := fixture.guard.Close(context.Background(), closed); err != nil {
		t.Fatal("关闭屏障无法幂等确认", err)
	}
}
