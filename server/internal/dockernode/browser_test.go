package dockernode

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type fakeComputerBrowsers struct{ ensureError, retireError error }

func (b *fakeComputerBrowsers) ensure(context.Context, Resource, container) error {
	return b.ensureError
}
func (b *fakeComputerBrowsers) retire(context.Context, Resource) error { return b.retireError }
func (*fakeComputerBrowsers) shutdown(context.Context) error           { return nil }

func testBrowserOptions() BrowserOptions {
	return BrowserOptions{CallTimeout: time.Second, CleanupTimeout: 3 * time.Second, ShutdownTimeout: 5 * time.Second, Groups: testProbeOptions().Groups, Limits: computeragent.BrowserLimits{Width: 1280, Height: 800, MaximumWindows: 8, MaximumPerSession: 4, MaximumWindowRecords: 16, MaximumEvents: 32, CallTimeoutMS: 1000, DialogTimeoutMS: 10000, ShutdownTimeoutMS: 1000}}
}

func newBrowserFixture(t *testing.T) (fixture, *nodeBrowsers, *probeGroups, Resource, container, *[]string) {
	t.Helper()
	f, probes, groups, resource, current := newProbeFixture(t)
	browsers := &nodeBrowsers{node: f.node, store: f.node.journal, groups: groups, options: testBrowserOptions(), logger: sessionLogger(), locate: probes.locate, stamp: probes.stamp}
	actions := []string{}
	browsers.invoke = func(ctx context.Context, _ Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
		actions = append(actions, action)
		state, err := browsers.store.readBrowser(resource.Specification.UserID)
		if err != nil || state == nil || state.Grant != grant {
			t.Fatal("调用未绑定持久授权", state, err)
		}
		phase := "active"
		switch action {
		case "start":
			if state.Phase != "dispatched" || len(files) != 3 {
				t.Fatal("未持久消费就启动", state)
			}
		case "status":
			if state.Phase != "active" || len(files) != 0 {
				t.Fatal("查询错误实例", state)
			}
		case "close":
			phase = "closed"
			if len(files) != 0 {
				t.Fatal("关闭泄漏入口")
			}
		default:
			t.Fatal("未知浏览器动作", action)
		}
		return computeragent.BrowserObservation{Grant: grant, Phase: phase}, ctx.Err()
	}
	return f, browsers, groups, resource, current, &actions
}

func TestNodeBrowserLifecycleAndMonotonicVersions(t *testing.T) {
	f, b, g, r, current, actions := newBrowserFixture(t)
	g.beforeCreate = func(ref processgroup.ServiceReference) {
		state, err := f.node.journal.readBrowser(r.Specification.UserID)
		if err != nil || state == nil || state.Phase != "prepared" || state.Grant.Request.Group != ref {
			t.Fatal("先创建后保存意图", state, err)
		}
	}
	for revision := int64(1); revision <= 2; revision++ {
		if err := b.ensure(t.Context(), r, current); err != nil {
			t.Fatal(err)
		}
		if err := b.ensure(t.Context(), r, current); err != nil {
			t.Fatal(err)
		}
		state, err := f.node.journal.readBrowser(r.Specification.UserID)
		if err != nil || state.Phase != "active" || state.Grant.Revision != revision {
			t.Fatal(state, err)
		}
		for _, file := range g.files {
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("宿主入口泄漏", err)
			}
		}
		if err := b.retire(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if err := b.retire(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		state, err = f.node.journal.readBrowser(r.Specification.UserID)
		if err != nil || state.Phase != "closed" || state.Closure != "agent" {
			t.Fatal(state, err)
		}
	}
	if len(g.created) != 2 || len(g.drained) != 2 || g.created[0].ID == g.created[1].ID || len(*actions) != 6 {
		t.Fatal("启动重放或未回收", *actions)
	}
}

type faultyBrowserStore struct {
	browserStore
	phase string
}

func (s *faultyBrowserStore) saveBrowser(state *browserRecord) error {
	if state.Phase == s.phase {
		return errors.New("测试浏览器刷盘失败")
	}
	return s.browserStore.saveBrowser(state)
}

func TestNodeBrowserFailuresCloseOriginalGrant(t *testing.T) {
	for _, stage := range []string{"prepared", "create", "controls", "dispatched", "start", "response", "active", "closing", "drain", "closed", "cancelled"} {
		t.Run(stage, func(t *testing.T) {
			f, b, g, r, current, actions := newBrowserFixture(t)
			fault := errors.New("测试故障")
			b.store = &faultyBrowserStore{browserStore: b.store, phase: stage}
			if stage == "create" {
				g.createError = fault
			}
			if stage == "controls" {
				g.controlsError = fault
			}
			if stage == "drain" {
				g.drainError = fault
			}
			invoke := b.invoke
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b.invoke = func(ctx context.Context, r Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
				observed, err := invoke(ctx, r, grant, action, files)
				if action == "start" {
					if stage == "start" {
						return computeragent.BrowserObservation{}, fault
					}
					if stage == "response" {
						observed.Grant.Revision++
					}
					if stage == "cancelled" {
						cancel()
					}
					if stage == "closing" || stage == "drain" || stage == "closed" {
						return computeragent.BrowserObservation{}, fault
					}
				}
				return observed, err
			}
			g.beforeDrain = func(ctx context.Context, _ processgroup.ServiceReference) {
				if ctx.Err() != nil {
					t.Fatal("取消影响独立回收")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("回收无时限")
				}
			}
			if err := b.ensure(ctx, r, current); err == nil {
				t.Fatal("失败仍确认活动")
			}
			if stage == "prepared" {
				if len(g.created)+len(g.drained)+len(*actions) != 0 {
					t.Fatal("准备失败仍访问资源")
				}
				return
			}
			if len(g.created) != 1 || len(g.drained) != 1 || g.created[0] != g.drained[0] {
				t.Fatal("没有回收原资源组")
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil || state == nil {
				t.Fatal(state, err)
			}
			uncertain := stage == "closing" || stage == "drain" || stage == "closed"
			if (state.Phase != "closed") != uncertain {
				t.Fatal("错误关闭屏障", state)
			}
			starts := 0
			for _, action := range *actions {
				if action == "start" {
					starts++
				}
			}
			if starts > 1 {
				t.Fatal("重放未知启动")
			}
		})
	}
}

func TestNodeBrowserUnknownCloseIsolatesBeforeDraining(t *testing.T) {
	f, b, g, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	invoke := b.invoke
	b.invoke = func(ctx context.Context, r Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
		if action == "close" {
			return computeragent.BrowserObservation{}, errors.New("关闭回包丢失")
		}
		return invoke(ctx, r, grant, action, files)
	}
	g.beforeDrain = func(ctx context.Context, _ processgroup.ServiceReference) {
		actual, err := f.engine.inspectContainer(ctx, current.ID)
		if err != nil || actual.State.Running {
			t.Fatal("代理未关闭也未隔离就回收", err)
		}
	}
	if err := b.retire(t.Context(), r); err == nil {
		t.Fatal("隔离冒充正常关闭")
	}
	state, err := f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil || state.Phase != "closed" || state.Closure != "isolated" {
		t.Fatal(state, err)
	}
	if err := b.retire(t.Context(), r); err != nil {
		t.Fatal("隔离确认后无法完成后续对账", err)
	}
}

func TestNodeBrowserRecoveryNeverStarts(t *testing.T) {
	for _, phase := range []string{"prepared", "dispatched", "active", "closing", "closed"} {
		t.Run(phase, func(t *testing.T) {
			f, b, g, r, current, _ := newBrowserFixture(t)
			if err := b.ensure(t.Context(), r, current); err != nil {
				t.Fatal(err)
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = phase
			if phase == "closed" {
				state.Closure = "agent"
			}
			encoded, err := agentchannel.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.node.journal.writeFile(r.Specification.UserID+".browser.json", encoded); err != nil {
				t.Fatal(err)
			}
			invoke := b.invoke
			b.invoke = func(ctx context.Context, r Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
				if action != "close" {
					t.Fatal("恢复重放浏览器操作", action)
				}
				return invoke(ctx, r, grant, action, files)
			}
			if err := b.recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(g.created) != 1 {
				t.Fatal("恢复建立了新实例")
			}
			state, err = f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil || state.Phase != "closed" || state.Grant.Revision != 1 {
				t.Fatal(state, err)
			}
		})
	}
}

func TestNodeBrowserStatusFailureDoesNotRestart(t *testing.T) {
	_, b, g, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	invoke := b.invoke
	b.invoke = func(ctx context.Context, r Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
		if action == "status" {
			return computeragent.BrowserObservation{}, errors.New("查询失败")
		}
		return invoke(ctx, r, grant, action, files)
	}
	if err := b.ensure(t.Context(), r, current); err == nil || len(g.created) != 1 || len(g.drained) != 1 {
		t.Fatal("查询失败仍重启", err)
	}
}

func TestNodeBrowserStatusRechecksParentAfterResponse(t *testing.T) {
	f, b, g, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	invoke := b.invoke
	b.invoke = func(ctx context.Context, r Resource, grant computeragent.BrowserGrant, action string, files []*os.File) (computeragent.BrowserObservation, error) {
		observed, err := invoke(ctx, r, grant, action, files)
		if action == "status" {
			locate := b.locate
			b.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
				ref, err := locate(ctx, id, pid)
				ref.Inode++
				return ref, err
			}
		}
		return observed, err
	}
	if err := b.ensure(t.Context(), r, current); err == nil {
		t.Fatal("回包后父组漂移仍确认活动")
	}
	state, err := f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil || state.Phase != "closing" || len(g.created) != 1 || len(g.drained) != 0 {
		t.Fatal("漂移后重启或回收了错误组", state, err)
	}
}

func TestNodeBrowserRejectsInvalidRecordsAndExhaustion(t *testing.T) {
	for _, change := range []string{"future", "owner", "exhausted", "damaged", "symlink"} {
		t.Run(change, func(t *testing.T) {
			f, b, g, r, current, _ := newBrowserFixture(t)
			if err := b.ensure(t.Context(), r, current); err != nil {
				t.Fatal(err)
			}
			if err := b.retire(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "future":
				state.Specification.Generation++
				state.Grant.Identity.Generation++
			case "owner":
				state.Grant.Identity.ComputerID = ids.New()
			case "exhausted":
				state.Grant.Revision = math.MaxInt64
			}
			encoded, err := agentchannel.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			if change == "damaged" {
				encoded = []byte("{}")
			}
			path := filepath.Join(r.ControlPath, "..", r.Specification.UserID+".browser.json")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if change == "symlink" {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.ensure(t.Context(), r, current); err == nil || len(g.created) != 1 {
				t.Fatal("无效记录未拒绝", err)
			}
		})
	}
}

func TestNodeBrowserBlocksRuntimeAdmissionAndRemoval(t *testing.T) {
	f := newFixture(t)
	b := f.node.browsers.(*fakeComputerBrowsers)
	b.ensureError = errors.New("浏览器未启动")
	if _, err := f.node.Ensure(t.Context(), f.specification); err == nil {
		t.Fatal("浏览器失败仍开户")
	}
	b.ensureError = nil
	if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	b.retireError = errors.New("浏览器未回收")
	if _, err := f.node.Remove(t.Context(), f.specification); err == nil {
		t.Fatal("未回收仍删除")
	}
	current, err := f.engine.inspectContainer(t.Context(), f.node.resource(f.specification).Name)
	if err != nil || !current.State.Running || f.storage.removals != 0 {
		t.Fatal("失败后仍删除资源", err)
	}
}

func TestNodeBrowserConcurrentReconciliationStartsOnce(t *testing.T) {
	f, b, g, _, _, actions := newBrowserFixture(t)
	f.node.browsers = b
	var pending sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		pending.Add(1)
		go func() { defer pending.Done(); _, err := f.node.Inspect(t.Context(), f.specification); results <- err }()
	}
	pending.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	starts := 0
	for _, action := range *actions {
		if action == "start" {
			starts++
		}
	}
	if starts != 1 || len(g.created) != 1 {
		t.Fatal("并发对账重复启动", starts, len(g.created))
	}
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	if len(g.drained) != 1 {
		t.Fatal("删除容器未回收浏览器")
	}
}

func TestNodeBrowserRetirementRetainsHighestRevision(t *testing.T) {
	f, b, _, r, current, _ := newBrowserFixture(t)
	if err := b.ensure(t.Context(), r, current); err != nil {
		t.Fatal(err)
	}
	state, err := f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	observed := computeragent.BrowserSnapshot{Version: 1, Grant: state.Grant, Phase: "active"}
	encoded, err := agentchannel.Encode(observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.ControlPath, "browser.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	f.node.browsers = b
	if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	before, err := f.node.journal.readFile(r.Specification.UserID + ".browser.json")
	if err != nil {
		t.Fatal(err)
	}
	next := advanceSpecification(f.specification)
	if _, err := f.node.Inspect(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(r.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧控制目录未回收", err)
	}
	after, err := f.node.journal.readFile(r.Specification.UserID + ".browser.json")
	if err != nil || string(before) != string(after) {
		t.Fatal("最高浏览器版本被删除或修改", err)
	}
	// 生命周期替身不创建真实 PID，先按既有夹具建立下一代，再测试浏览器的跨代版本。
	f.node.browsers = &fakeComputerBrowsers{}
	f.prepareComputer(t, next)
	f.node.browsers = b
	if _, err := f.node.Inspect(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	state, err = f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil || state.Grant.Revision != 2 || state.Grant.Identity.Generation != next.Generation {
		t.Fatal("换代重置版本", state, err)
	}
}

func TestNodeBrowserRetirementRejectsUnconfirmedAgentRecord(t *testing.T) {
	for _, scenario := range []string{"missing", "revision", "grant", "identity", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, _, r, current, _ := newBrowserFixture(t)
			if err := b.ensure(t.Context(), r, current); err != nil {
				t.Fatal(err)
			}
			if err := b.retire(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			observed := computeragent.BrowserSnapshot{Version: 1, Grant: state.Grant, Phase: "closed"}
			switch scenario {
			case "missing":
				if err := f.node.journal.root.Remove(r.Specification.UserID + ".browser.json"); err != nil {
					t.Fatal(err)
				}
			case "revision":
				observed.Grant.Revision++
			case "grant":
				observed.Grant.Limits.Width--
			case "identity":
				observed.Grant.Identity.NodeID = ids.New()
			}
			encoded, err := agentchannel.Encode(observed)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" {
				encoded = []byte("{}")
			}
			if err := os.WriteFile(filepath.Join(r.ControlPath, "browser.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.node.Remove(t.Context(), f.specification); err != nil {
				t.Fatal(err)
			}
			if _, err := f.node.Inspect(t.Context(), advanceSpecification(f.specification)); err == nil {
				t.Fatal("未核对宿主关闭屏障就删除代理记录")
			}
			retained, err := os.ReadFile(filepath.Join(r.ControlPath, "browser.json"))
			if err != nil || string(retained) != string(encoded) {
				t.Fatal("失败破坏代理记录", err)
			}
		})
	}
}

func TestNodeBrowserIsolationFailureAndContainerDriftPreserveBarrier(t *testing.T) {
	for _, scenario := range []string{"stop", "parent", "container"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, g, r, current, _ := newBrowserFixture(t)
			if err := b.ensure(t.Context(), r, current); err != nil {
				t.Fatal(err)
			}
			b.invoke = func(context.Context, Resource, computeragent.BrowserGrant, string, []*os.File) (computeragent.BrowserObservation, error) {
				return computeragent.BrowserObservation{}, errors.New("代理失联")
			}
			switch scenario {
			case "stop":
				f.engine.stopError = errors.New("隔离未确认")
			case "parent":
				locate := b.locate
				b.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
					ref, err := locate(ctx, id, pid)
					ref.Inode++
					return ref, err
				}
			case "container":
				f.engine.mu.Lock()
				changed := f.engine.containers[r.Name]
				changed.Config.Labels = map[string]string{}
				f.engine.containers[r.Name] = changed
				f.engine.mu.Unlock()
			}
			if err := b.retire(t.Context(), r); err == nil || len(g.drained) != 0 {
				t.Fatal("未隔离或错误实例仍被回收", err)
			}
			state, err := f.node.journal.readBrowser(r.Specification.UserID)
			if err != nil || state.Phase != "closing" {
				t.Fatal("丢失未知关闭屏障", state, err)
			}
		})
	}
}
