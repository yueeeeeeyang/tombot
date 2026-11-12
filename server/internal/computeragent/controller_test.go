package computeragent

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type memoryCatalog struct {
	mu      sync.Mutex
	records map[string]*memoryStore
}

func (catalog *memoryCatalog) Desktop(id string) (Store, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if catalog.records == nil {
		catalog.records = map[string]*memoryStore{}
	}
	if catalog.records[id] == nil {
		catalog.records[id] = &memoryStore{}
	}
	return catalog.records[id], nil
}

func (catalog *memoryCatalog) Visit(ctx context.Context, visit func(Snapshot) error) error {
	catalog.mu.Lock()
	records := make([]*memoryStore, 0, len(catalog.records))
	for _, record := range catalog.records {
		records = append(records, record)
	}
	catalog.mu.Unlock()
	for _, record := range records {
		snapshot, err := record.Load(ctx)
		if err != nil {
			return err
		}
		if snapshot != nil {
			if err = visit(*snapshot); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func desktopCommand(command computer.SessionCommand, display int) (computer.SessionCommand, processgroup.Reference) {
	command.Session.DesktopID, command.Session.BotID = ids.New(), ids.New()
	command.Session.SessionID, command.Session.RunID = ids.New(), ids.New()
	command.Session.UID, command.Session.DisplayNumber = computer.DesktopUIDBase+display, display
	command.Session.FencingToken, command.Revision = 1, 1
	return command, processgroup.Reference{Session: command.Session, ParentPath: "system.slice/docker-" + command.Session.ContainerID + ".scope", ParentDevice: 1, ParentInode: 2}
}

func requireAgentPhase(t *testing.T, controller *Controller, command computer.SessionCommand, phase string) {
	t.Helper()
	record, err := controller.store.Desktop(command.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := record.Load(context.Background())
	if err != nil || state == nil || state.Command != command || state.Phase != phase {
		t.Fatalf("桌面持久状态不符：%v，%v", state, err)
	}
}

func serveDesktops(t *testing.T, controller *Controller) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("桌面看门狗没有停止")
		}
	})
	return done
}

func TestControllerDesktopIsolationAndIdentity(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	controller := fixture.controller
	first := fixture.command
	second, reference := desktopCommand(first, 2)
	for _, input := range []struct {
		command   computer.SessionCommand
		reference processgroup.Reference
	}{{first, fixture.reference}, {second, reference}} {
		if _, err := controller.Open(ctx, input.command, input.reference, fixture.files(t)); err != nil {
			t.Fatal(err)
		}
	}
	guard, err := controller.desktop(ctx, second.Session.DesktopIdentity, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := guard.permit.deadline
	if _, err = controller.Close(ctx, first); err != nil {
		t.Fatal(err)
	}
	if !guard.permit.active(second.Revision) || guard.permit.deadline != deadline {
		t.Fatal("关闭其他桌面撤销或改变了当前授权")
	}
	called := false
	if err = controller.WithTask(ctx, second.Session, "shell", func(_ context.Context, actual processgroup.Reference, _, _ *os.File) error {
		called = true
		if actual != reference {
			return errors.New("借出了其他桌面的执行入口")
		}
		return nil
	}); err != nil || !called {
		t.Fatal(err)
	}
	second.Revision++
	if _, err = controller.Renew(ctx, second); err != nil {
		t.Fatal(err)
	}
	requireAgentPhase(t, controller, first, "closed")
	requireAgentPhase(t, controller, second, "active")
	for _, change := range []func(*computer.ExecutionSession){
		func(session *computer.ExecutionSession) { session.BotID = ids.New() },
		func(session *computer.ExecutionSession) { session.DesktopID = ids.New() },
		func(session *computer.ExecutionSession) { session.UID++; session.DisplayNumber++ },
		func(session *computer.ExecutionSession) { session.UserID = ids.New() },
	} {
		forged := second
		change(&forged.Session)
		if _, err = controller.Close(ctx, forged); err == nil {
			t.Fatal("替换桌面归属关闭成功")
		}
		requireAgentPhase(t, controller, second, "active")
	}
	conflict, conflictReference := desktopCommand(first, 2)
	files := fixture.files(t)
	if _, err = controller.Open(ctx, conflict, conflictReference, files); err == nil {
		t.Fatal("两个桌面共用物理显示身份")
	}
	for _, file := range files {
		if _, err = file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("拒绝桌面未关闭控制描述符", err)
		}
	}
	next := first
	next.Session.SessionID, next.Session.RunID = ids.New(), ids.New()
	next.Session.FencingToken++
	nextReference := fixture.reference
	nextReference.Session = next.Session
	if _, err = controller.Open(ctx, next, nextReference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = controller.Close(ctx, first); err == nil {
		t.Fatal("迟到关闭命中新桌面会话")
	}
	requireAgentPhase(t, controller, next, "active")
	requireAgentPhase(t, controller, second, "active")
}

func TestControllerBlockedDesktopDoesNotBlockOthers(t *testing.T) {
	for _, phase := range []string{"启动回调", "关闭刷盘"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newFixture(t)
			ctx := context.Background()
			fixture.open(t)
			controller := fixture.controller
			second, reference := desktopCommand(fixture.command, 2)
			if _, err := controller.Open(ctx, second, reference, fixture.files(t)); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			done := make(chan error, 1)
			if phase == "启动回调" {
				go func() {
					done <- controller.WithTask(ctx, fixture.command.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error {
						entered <- struct{}{}
						<-release
						return nil
					})
				}()
			} else {
				fixture.store.blockClosing, fixture.store.closingStarted = release, entered
				go func() { _, err := controller.Close(ctx, fixture.command); done <- err }()
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("未进入阻塞的桌面操作")
			}
			bounded, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			second.Revision++
			if _, err := controller.Renew(bounded, second); err != nil {
				t.Fatal("另一桌面续约被阻塞", err)
			}
			if err := controller.WithTask(bounded, second.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error { return nil }); err != nil {
				t.Fatal("另一桌面任务被阻塞", err)
			}
			third, thirdReference := desktopCommand(fixture.command, 3)
			if _, err := controller.Open(bounded, third, thirdReference, fixture.files(t)); err != nil {
				t.Fatal("第三个桌面初始化被阻塞", err)
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("原桌面操作未结束")
			}
			requireAgentPhase(t, controller, second, "active")
		})
	}
}

func TestControllerCapacityAndIndependentExpiry(t *testing.T) {
	fixture := newFixture(t)
	controller := fixture.controller
	controller.capacity.maximum = 2
	ctx := context.Background()
	serveDesktops(t, controller)
	first := fixture.command
	first.Lifetime = time.Second
	second, secondReference := desktopCommand(fixture.command, 2)
	third, thirdReference := desktopCommand(fixture.command, 3)
	if _, err := controller.Open(ctx, first, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Open(ctx, second, secondReference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Open(ctx, third, thirdReference, fixture.files(t)); err == nil {
		t.Fatal("同一用户超出桌面并发上限")
	}
	guard, err := controller.desktop(ctx, second.Session.DesktopIdentity, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		state, err := fixture.store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase == "closed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("目标桌面未独立到期回收")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !guard.permit.active(second.Revision) {
		t.Fatal("另一桌面被过期看门狗撤销")
	}
	// 持久提交可先于进程内收尾被观察到，等待目标守卫离开临界区后再核对释放名额。
	_, leave, err := fixture.guard.enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leave()
	if _, err := controller.Open(ctx, third, thirdReference, fixture.files(t)); err != nil {
		t.Fatal("回收没有释放单个桌面名额", err)
	}
	requireAgentPhase(t, controller, second, "active")
	first.Lifetime = 2 * time.Second
	if _, err := controller.Open(ctx, first, fixture.reference, fixture.files(t)); err == nil {
		t.Fatal("重复版本更换期限被接受")
	}
}

func TestControllerRecoveryRejectsUnknownAndConflictingDesktops(t *testing.T) {
	for _, scenario := range []string{"旧活动桌面", "重复显示", "重复机器人", "其他用户", "旧版本"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t)
			first := Snapshot{Version: 2, Command: fixture.command, Phase: "closed"}
			second, reference := desktopCommand(fixture.command, 2)
			other := Snapshot{Version: 2, Command: second, Phase: "closed"}
			switch scenario {
			case "旧活动桌面":
				other.Phase, other.Group = "active", &reference
			case "重复显示":
				other.Command.Session.UID, other.Command.Session.DisplayNumber = first.Command.Session.UID, first.Command.Session.DisplayNumber
			case "重复机器人":
				other.Command.Session.BotID = first.Command.Session.BotID
			case "其他用户":
				other.Command.Session.UserID = ids.New()
			case "旧版本":
				other.Version = 1
			}
			catalog := &memoryCatalog{records: map[string]*memoryStore{first.Command.Session.DesktopID: {state: &first}, other.Command.Session.DesktopID: {state: &other}}}
			if _, err := NewController(context.Background(), fixture.guard.identity, catalog, fixture.factory, fixture.guard.options, 2, fixture.guard.logger); err == nil {
				t.Fatal("恢复接受了未知或冲突的桌面")
			}
		})
	}
}

func TestControllerConcurrentAdmissionAndUnknownClose(t *testing.T) {
	fixture := newFixture(t)
	controller := fixture.controller
	controller.capacity.maximum = 2
	ctx := context.Background()
	type result struct {
		command computer.SessionCommand
		err     error
	}
	results, start := make(chan result, 4), make(chan struct{})
	for display := 2; display <= 5; display++ {
		command, reference := desktopCommand(fixture.command, display)
		files := fixture.files(t)
		go func() {
			<-start
			_, err := controller.Open(ctx, command, reference, files)
			results <- result{command, err}
		}()
	}
	close(start)
	var admitted []computer.SessionCommand
	for range 4 {
		outcome := <-results
		if outcome.err == nil {
			admitted = append(admitted, outcome.command)
		}
	}
	if len(admitted) != 2 {
		t.Fatal("并发桌面名额超发或被用户级串行限制", len(admitted))
	}
	first := admitted[0]
	record, err := controller.store.Desktop(first.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	store := record.(*memoryStore)
	store.failPhase, store.failCount = "closed", 1
	if _, err = controller.Close(ctx, first); err == nil {
		t.Fatal("关闭结果未知被当作成功")
	}
	controller.capacity.mu.Lock()
	_, reserved := controller.capacity.active[first.Session.DesktopID]
	controller.capacity.mu.Unlock()
	if !reserved {
		t.Fatal("未知持久关闭释放了活动名额")
	}
	second := admitted[1]
	second.Revision++
	if _, err = controller.Renew(ctx, second); err != nil {
		t.Fatal("其他桌面被关闭失败撤销", err)
	}
	if _, err = controller.Close(ctx, first); err != nil {
		t.Fatal(err)
	}
	controller.capacity.mu.Lock()
	count := len(controller.capacity.active)
	controller.capacity.mu.Unlock()
	if count != 1 {
		t.Fatal("确认关闭没有准确释放一个名额", count)
	}
}

func TestControllerFatalDesktopStopsNewOperations(t *testing.T) {
	fixture := newFixture(t)
	controller := fixture.controller
	first := fixture.command
	first.Lifetime = time.Second
	if _, err := controller.Open(context.Background(), first, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	fixture.factory.scopes[0].failRevoke.Store(true)
	second, reference := desktopCommand(fixture.command, 2)
	if _, err := controller.Open(context.Background(), second, reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("致命桌面错误未向入口传播")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("致命桌面错误被其他桌面阻塞")
	}
	called := false
	if err := controller.WithTask(context.Background(), second.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error { called = true; return nil }); err == nil || called {
		t.Fatal("容器失控后仍启动新任务")
	}
	cancel()
	// 致命通知早于后台清理完成；重置测试替身前须等待清理临界区退出。
	_, leave, err := fixture.guard.acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	leave()
	fixture.factory.scopes[0].failRevoke.Store(false)
}

func TestControllerShutdownBoundsPendingTasks(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	controller := fixture.controller
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- controller.WithTask(context.Background(), fixture.command.Session, "shell", func(context.Context, processgroup.Reference, *os.File, *os.File) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("任务回调未开始")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := controller.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("停服未等待在途任务或没有返回超时", err)
	}
	if _, err := controller.Renew(context.Background(), fixture.command); err == nil {
		t.Fatal("停服超时后重新开放了接单")
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("在途任务未结束")
	}
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAgentPhase(t, controller, fixture.command, "closed")
}

func TestControllerInitializationFailureDoesNotRevokeActiveDesktop(t *testing.T) {
	fixture := newFixture(t)
	fixture.open(t)
	ctx := context.Background()
	controller := fixture.controller
	second, reference := desktopCommand(fixture.command, 2)
	record, err := controller.store.Desktop(second.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	record.(*memoryStore).failLoadCount = 1
	if _, err = controller.Open(ctx, second, reference, fixture.files(t)); err == nil {
		t.Fatal("记录读取失败仍发放了授权")
	}
	first := fixture.command
	first.Revision++
	if _, err = controller.Renew(ctx, first); err != nil {
		t.Fatal("其他桌面因初始化读取失败被撤销", err)
	}
	if _, err = controller.Open(ctx, second, reference, fixture.files(t)); err != nil {
		t.Fatal("显式重试未重新核验持久记录", err)
	}
	requireAgentPhase(t, controller, first, "active")
	requireAgentPhase(t, controller, second, "active")
}
