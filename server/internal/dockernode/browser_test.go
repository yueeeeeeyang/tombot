package dockernode

import (
	"context"
	"errors"
	"io.yueyang/tom/internal/taskprocess"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type fakeComputerBrowsers struct{ ensureError, retireError, checkError error }

func (b *fakeComputerBrowsers) ensure(context.Context, Resource, container) error {
	return b.ensureError
}
func (b *fakeComputerBrowsers) retire(context.Context, Resource) error { return b.retireError }
func (*fakeComputerBrowsers) shutdown(context.Context) error           { return nil }

func testBrowserOptions() BrowserOptions {
	return BrowserOptions{CallTimeout: time.Second, CleanupTimeout: 3 * time.Second, ShutdownTimeout: 5 * time.Second, Groups: testProbeOptions().Groups}
}

func retiredBrowserLimits() computeragent.BrowserLimits {
	return computeragent.BrowserLimits{Width: 1280, Height: 800, MaximumWindows: 8, MaximumPerSession: 4, MaximumWindowRecords: 16, MaximumEvents: 32, CallTimeoutMS: 1000, DialogTimeoutMS: 10000, ShutdownTimeoutMS: 1000}
}

func newBrowserFixture(t *testing.T) (fixture, *nodeBrowsers, *probeGroups, Resource, container, *[]string) {
	t.Helper()
	f, probes, groups, resource, current := newProbeFixture(t)
	browsers := &nodeBrowsers{node: f.node, store: f.node.journal, groups: groups, options: testBrowserOptions(), logger: sessionLogger(), locate: probes.locate}
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
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= browsers.options.CallTimeout {
				t.Fatal("节点冷启动仍使用普通调用预算")
			}
		case "status":
			if state.Phase != "active" || len(files) != 0 {
				t.Fatal("查询错误实例", state)
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > browsers.options.CallTimeout {
				t.Fatal("普通查询使用了冷启动预算")
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

func TestNodeBrowserUnknownCloseIsolatesBeforeDraining(t *testing.T) {
	f, b, g, r, current, _ := newBrowserFixture(t)
	if err := seedRetiredBrowser(t.Context(), b, r, current); err != nil {
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
			if err := seedRetiredBrowser(t.Context(), b, r, current); err != nil {
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

func TestNodeBrowserBlocksRuntimeAdmissionAndRemoval(t *testing.T) {
	f := newFixture(t)
	b := f.node.browsers.(*fakeComputerBrowsers)
	b.ensureError = errors.New("浏览器未启动")
	if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
		t.Fatal("浏览器未启动不应阻塞开户", err)
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

func TestNodeBrowserRetirementRetainsHighestRevision(t *testing.T) {
	f, b, _, r, current, _ := newBrowserFixture(t)
	if err := seedRetiredBrowser(t.Context(), b, r, current); err != nil {
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
	// 新镜像不会为新用户或新一代电脑重新签发共享 Chrome 授权。
	f.node.browsers = &fakeComputerBrowsers{}
	f.prepareComputer(t, next)
	state, err = f.node.journal.readBrowser(r.Specification.UserID)
	if err != nil || state.Grant.Revision != 1 || state.Phase != "closed" {
		t.Fatal("换代不应启动共享浏览器", state, err)
	}

}

func TestNodeBrowserRetirementRejectsUnconfirmedAgentRecord(t *testing.T) {
	for _, scenario := range []string{"missing", "revision", "grant", "identity", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, _, r, current, _ := newBrowserFixture(t)
			if err := seedRetiredBrowser(t.Context(), b, r, current); err != nil {
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
			if err := seedRetiredBrowser(t.Context(), b, r, current); err != nil {
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

func (b *fakeComputerBrowsers) check(context.Context, Resource, container) error { return b.checkError }

// seedRetiredBrowser 建立旧部署留下的活动记录，供新节点验证升级排空，不启动 Chrome。
func seedRetiredBrowser(ctx context.Context, b *nodeBrowsers, r Resource, current container) error {
	parent, err := b.locate(ctx, current.ID, current.State.Pid)
	if err != nil {
		return err
	}
	group := processgroup.ServiceReference{Container: parent, ID: ids.New(), Kind: processgroup.BrowserService}
	grant := computeragent.BrowserGrant{Identity: b.node.agentConfiguration(r).Identity, Revision: 1, Request: taskprocess.BrowserRequest{Version: 1, Group: group, Deadline: agentchannel.Deadline{ExpiresNS: 1, TimeNamespace: 2}}, Limits: retiredBrowserLimits()}
	record := &browserRecord{Version: 1, Specification: r.Specification, Grant: grant}
	for _, phase := range []string{"prepared", "dispatched", "active"} {
		record.Phase = phase
		if err := b.store.saveBrowser(record); err != nil {
			return err
		}
	}
	return b.groups.Create(ctx, group)
}
