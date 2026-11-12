package dockernode

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type fakeComputerProbes struct {
	checkError  error
	retireError error
}

func (probes *fakeComputerProbes) check(context.Context, Resource, container) (computerprobe.Evidence, error) {
	return probeEvidence(ids.New()), probes.checkError
}
func (probes *fakeComputerProbes) retire(context.Context, Resource) error { return probes.retireError }

func testProbeOptions() ProbeOptions {
	return ProbeOptions{CallTimeout: time.Second, CleanupTimeout: time.Second, Groups: processgroup.Options{GracePeriod: 10 * time.Millisecond, PollInterval: time.Millisecond, MaximumProcesses: 64}}
}

func probeEvidence(id string) computerprobe.Evidence {
	return computerprobe.Evidence{ServiceID: id, Terminal: true, Files: true, LocalNetwork: true, FilesystemType: 0xef53, FilesystemSize: 2 << 30}
}

type probeGroups struct {
	t                                      *testing.T
	createError, controlsError, drainError error
	created, drained                       []processgroup.ServiceReference
	files                                  []*os.File
	beforeCreate                           func(processgroup.ServiceReference)
	beforeDrain                            func(context.Context, processgroup.ServiceReference)
}

func (groups *probeGroups) Create(_ context.Context, ref processgroup.ServiceReference) error {
	groups.created = append(groups.created, ref)
	if groups.beforeCreate != nil {
		groups.beforeCreate(ref)
	}
	return groups.createError
}
func (groups *probeGroups) Controls(context.Context, processgroup.ServiceReference) ([]*os.File, error) {
	if groups.controlsError != nil {
		return nil, groups.controlsError
	}
	groups.files = nil
	for range 3 {
		file, err := os.Open(os.DevNull)
		if err != nil {
			groups.t.Fatal(err)
		}
		groups.files = append(groups.files, file)
	}
	return groups.files, nil
}
func (*probeGroups) Contains(context.Context, processgroup.ServiceReference, int) (bool, error) {
	return false, errors.New("测试探针不使用进程成员查询")
}
func (groups *probeGroups) Drain(ctx context.Context, ref processgroup.ServiceReference) error {
	groups.drained = append(groups.drained, ref)
	if groups.beforeDrain != nil {
		groups.beforeDrain(ctx, ref)
	}
	return groups.drainError
}

func newProbeFixture(t *testing.T) (fixture, *nodeProbes, *probeGroups, Resource, container) {
	t.Helper()
	f := newFixture(t)
	session := f.prepareComputer(t, f.specification)
	resource := f.node.resource(f.specification)
	current, err := f.engine.inspectContainer(t.Context(), session.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	groups := &probeGroups{t: t}
	probes := &nodeProbes{node: f.node, store: f.node.journal, groups: groups, options: testProbeOptions(), logger: sessionLogger()}
	probes.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
		return processgroup.ContainerReference{ContainerID: id, Path: "system.slice/docker-" + id + ".scope", Device: 1, Inode: 2}, ctx.Err()
	}
	probes.stamp = func(time.Time) (agentchannel.Deadline, error) {
		return agentchannel.Deadline{ExpiresNS: 1, TimeNamespace: 2}, nil
	}
	probes.invoke = func(ctx context.Context, _ Resource, grant computeragent.ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
		state, err := probes.store.readProbe(resource.Specification.UserID)
		if err != nil || state == nil || state.Phase != "dispatched" || state.Grant != grant || len(files) != 3 {
			t.Fatal("未持久消费屏障就发送请求", state, err)
		}
		return probeEvidence(grant.Request.Group.ID), ctx.Err()
	}
	return f, probes, groups, resource, current
}

func TestNodeProbeCompleteAndMonotonic(t *testing.T) {
	f, probes, groups, resource, current := newProbeFixture(t)
	groups.beforeCreate = func(ref processgroup.ServiceReference) {
		state, err := f.node.journal.readProbe(resource.Specification.UserID)
		if err != nil || state == nil || state.Phase != "prepared" || state.Grant.Request.Group != ref {
			t.Fatal("未持久准备意图就创建进程组", state, err)
		}
	}
	for revision := int64(1); revision <= 2; revision++ {
		evidence, err := probes.check(t.Context(), resource, current)
		if err != nil || evidence.Validate(groups.created[len(groups.created)-1].ID) != nil {
			t.Fatal(evidence, err)
		}
		state, err := f.node.journal.readProbe(resource.Specification.UserID)
		if err != nil || state.Phase != "closed" || state.Grant.Revision != revision {
			t.Fatal(state, err)
		}
		for _, file := range groups.files {
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("宿主控制入口泄漏", err)
			}
		}
	}
	if !reflect.DeepEqual(groups.created, groups.drained) || groups.created[0].ID == groups.created[1].ID {
		t.Fatal("实例复用或回收对象错误")
	}
}

type faultyProbeStore struct {
	probeStore
	phase string
	saves int
}

func (store *faultyProbeStore) saveProbe(state *probeRecord) error {
	store.saves++
	if state.Phase == store.phase {
		return errors.New("测试记录写入失败")
	}
	return store.probeStore.saveProbe(state)
}

func TestNodeProbeFailureAlwaysCloses(t *testing.T) {
	for _, stage := range []string{"prepared", "create", "controls", "dispatched", "rpc", "evidence", "closing", "drain", "closed", "changed", "cancelled"} {
		t.Run(stage, func(t *testing.T) {
			f, probes, groups, resource, current := newProbeFixture(t)
			injected := errors.New("测试故障")
			calls := 0
			invoke := probes.invoke
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probes.store = &faultyProbeStore{probeStore: probes.store, phase: stage}
			if stage == "create" {
				groups.createError = injected
			}
			if stage == "controls" {
				groups.controlsError = injected
			}
			if stage == "drain" {
				groups.drainError = injected
			}
			probes.invoke = func(ctx context.Context, resource Resource, grant computeragent.ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
				calls++
				if stage == "rpc" {
					return computerprobe.Evidence{}, injected
				}
				evidence, err := invoke(ctx, resource, grant, files)
				if stage == "evidence" {
					evidence.ServiceID = ids.New()
				}
				if stage == "changed" {
					probes.locate = func(context.Context, string, int) (processgroup.ContainerReference, error) {
						ref := grant.Request.Group.Container
						ref.Inode++
						return ref, nil
					}
				}
				if stage == "cancelled" {
					cancel()
				}
				return evidence, err
			}
			groups.beforeDrain = func(ctx context.Context, _ processgroup.ServiceReference) {
				if ctx.Err() != nil {
					t.Fatal("业务取消传播到独立回收")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("回收没有时限")
				}
			}
			evidence, err := probes.check(ctx, resource, current)
			if err == nil || evidence != (computerprobe.Evidence{}) {
				t.Fatal("失败仍返回证据", evidence, err)
			}
			if calls > 1 {
				t.Fatal("未知请求被重试")
			}
			if stage == "prepared" {
				if len(groups.created)+len(groups.drained)+calls != 0 {
					t.Fatal("准备失败仍访问资源")
				}
				return
			}
			if len(groups.drained) != 1 || groups.drained[0] != groups.created[0] {
				t.Fatal("未回收原实例")
			}
			state, err := f.node.journal.readProbe(resource.Specification.UserID)
			if err != nil || state == nil {
				t.Fatal(state, err)
			}
			uncertain := stage == "closing" || stage == "drain" || stage == "closed"
			if (state.Phase != "closed") != uncertain {
				t.Fatal("回收记录与结果不一致", state.Phase)
			}
		})
	}
}

func TestNodeProbeRecoveryNeverReplays(t *testing.T) {
	for _, phase := range []string{"prepared", "dispatched", "closing", "closed"} {
		t.Run(phase, func(t *testing.T) {
			f, probes, groups, resource, current := newProbeFixture(t)
			if _, err := probes.check(t.Context(), resource, current); err != nil {
				t.Fatal(err)
			}
			state, err := f.node.journal.readProbe(resource.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = phase
			// 模拟断电时的已刷盘状态；生产写入口仍禁止反向转换。
			encoded, err := agentchannel.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.node.journal.writeFile(resource.Specification.UserID+".probe.json", encoded); err != nil {
				t.Fatal(err)
			}
			groups.drained = nil
			probes.invoke = func(context.Context, Resource, computeragent.ProbeGrant, []*os.File) (computerprobe.Evidence, error) {
				t.Fatal("恢复重放请求")
				return computerprobe.Evidence{}, nil
			}
			if err := probes.recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			expected := 1
			if phase == "closed" {
				expected = 0
			}
			if len(groups.drained) != expected {
				t.Fatal("恢复没有处理遗留实例")
			}
			state, err = f.node.journal.readProbe(resource.Specification.UserID)
			if err != nil || state.Phase != "closed" || state.Grant.Revision != 1 {
				t.Fatal(state, err)
			}
			if err := f.node.restoreEgress(t.Context()); err != nil {
				t.Fatal("探针记录污染出网扫描", err)
			}
		})
	}
}

func TestNodeProbeRecoveryRejectsUncertainOwnership(t *testing.T) {
	for _, kind := range []string{"missing", "future", "foreign", "drain"} {
		t.Run(kind, func(t *testing.T) {
			f, probes, groups, resource, current := newProbeFixture(t)
			groups.drainError = errors.New("测试未清空")
			if _, err := probes.check(t.Context(), resource, current); err == nil {
				t.Fatal("应保留未确认记录")
			}
			groups.drained = nil
			if kind != "drain" {
				groups.drainError = nil
			}
			state, err := f.node.journal.read(resource.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				if err := f.node.journal.root.Remove(state.Specification.UserID + ".json"); err != nil {
					t.Fatal(err)
				}
			case "future", "foreign":
				if kind == "future" {
					state.Specification.Generation++
				} else {
					state.Specification.ComputerID = ids.New()
				}
				probeState, err := f.node.journal.readProbe(resource.Specification.UserID)
				if err != nil {
					t.Fatal(err)
				}
				probeState.Specification = state.Specification
				probeState.Grant.Identity.Generation = state.Specification.Generation
				probeState.Grant.Identity.ComputerID = state.Specification.ComputerID
				encoded, err := agentchannel.Encode(probeState)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.node.journal.writeFile(resource.Specification.UserID+".probe.json", encoded); err != nil {
					t.Fatal(err)
				}
			}
			if err := probes.recover(t.Context()); err == nil {
				t.Fatal("恢复放行未知身份或未回收资源")
			}
			if kind != "drain" && len(groups.drained) != 0 {
				t.Fatal("归属未核实便操作资源")
			}
		})
	}
}

func TestNodeProbeJournalProtectsBarrier(t *testing.T) {
	for _, kind := range []string{"revision", "reuse", "phase", "corrupt", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			f, probes, _, resource, current := newProbeFixture(t)
			if _, err := probes.check(t.Context(), resource, current); err != nil {
				t.Fatal(err)
			}
			state, err := f.node.journal.readProbe(resource.Specification.UserID)
			if err != nil {
				t.Fatal(err)
			}
			name := resource.Specification.UserID + ".probe.json"
			path := filepath.Join(f.statePath, name)
			switch kind {
			case "revision":
				state.Grant.Revision--
				state.Phase = "prepared"
			case "reuse":
				state.Grant.Revision++
				state.Phase = "prepared"
			case "phase":
				state.Phase = "dispatched"
			case "corrupt":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.node.journal.saveProbe(state); err == nil {
				t.Fatal("危险屏障被覆盖")
			}
		})
	}
}

func TestNodeProbeRevisionSurvivesGeneration(t *testing.T) {
	f, probes, groups, resource, current := newProbeFixture(t)
	if _, err := probes.check(t.Context(), resource, current); err != nil {
		t.Fatal(err)
	}
	state, err := f.node.journal.readProbe(resource.Specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	resource.Specification.Generation++
	resource.Specification.Attempt++
	previous, err := probes.previous(resource)
	if err != nil || previous.Grant != state.Grant {
		t.Fatal("新代次丢失探针版本", err)
	}
	state.Grant.Revision = math.MaxInt64
	encoded, err := agentchannel.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.node.journal.writeFile(resource.Specification.UserID+".probe.json", encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := probes.check(t.Context(), resource, current); err == nil || len(groups.created) != 1 {
		t.Fatal("探针版本溢出继续执行", err)
	}
}

func TestNodeProbeLifecycleGate(t *testing.T) {
	f := newFixture(t)
	if _, err := f.node.Ensure(t.Context(), f.specification); err != nil {
		t.Fatal(err)
	}
	f.node.probes = &fakeComputerProbes{checkError: errors.New("测试探针失败"), retireError: errors.New("测试探针未回收")}
	if observed, err := f.node.Inspect(t.Context(), f.specification); err == nil || observed.Capabilities.Ready() {
		t.Fatal("基础探针失败仍就绪")
	}
	f.engine.events = nil
	if _, err := f.node.Remove(t.Context(), f.specification); err == nil {
		t.Fatal("遗留探针未回收仍删除容器")
	}
	if len(f.engine.events) != 0 {
		t.Fatal("回收未确认即停止或删除容器", f.engine.events)
	}
}

func TestNodeProbeSharesLifecycleLock(t *testing.T) {
	f, probes, _, _, _ := newProbeFixture(t)
	f.node.probes = probes
	entered, resume := make(chan struct{}), make(chan struct{})
	invoke := probes.invoke
	probes.invoke = func(ctx context.Context, resource Resource, grant computeragent.ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
		close(entered)
		select {
		case <-resume:
			return invoke(ctx, resource, grant, files)
		case <-ctx.Done():
			return computerprobe.Evidence{}, ctx.Err()
		}
	}
	inspected, removed := make(chan error, 1), make(chan error, 1)
	go func() { _, err := f.node.Inspect(t.Context(), f.specification); inspected <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("探针没有开始")
	}
	go func() { _, err := f.node.Remove(t.Context(), f.specification); removed <- err }()
	select {
	case err := <-removed:
		close(resume)
		t.Fatal("探针执行中删除越过生命周期锁", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(resume)
	for _, result := range []<-chan error{inspected, removed} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("生命周期操作未结束")
		}
	}
}

func TestNodeProbeUnknownResultUsesNewVersion(t *testing.T) {
	f, probes, groups, resource, current := newProbeFixture(t)
	invoke := probes.invoke
	probes.invoke = func(context.Context, Resource, computeragent.ProbeGrant, []*os.File) (computerprobe.Evidence, error) {
		return computerprobe.Evidence{}, context.DeadlineExceeded
	}
	groups.drainError = errors.New("测试内核暂未确认")
	if _, err := probes.check(t.Context(), resource, current); err == nil {
		t.Fatal("未知结果被当作成功")
	}
	first := groups.created[0]
	if _, err := probes.check(t.Context(), resource, current); err == nil || len(groups.created) != 1 {
		t.Fatal("旧组未回收便发起新检查")
	}
	groups.drainError = nil
	probes.invoke = invoke
	if _, err := probes.check(t.Context(), resource, current); err != nil {
		t.Fatal(err)
	}
	state, err := f.node.journal.readProbe(resource.Specification.UserID)
	if err != nil || state.Phase != "closed" || state.Grant.Revision != 2 || state.Grant.Request.Group.ID == first.ID || len(groups.created) != 2 {
		t.Fatal("新检查复用了未知请求", state, err)
	}
}

func TestNodeProbeRejectsContainerDrift(t *testing.T) {
	for _, kind := range []string{"stopped", "pid", "name", "label", "configuration", "parent", "clock"} {
		t.Run(kind, func(t *testing.T) {
			f, probes, groups, resource, current := newProbeFixture(t)
			changed := current
			switch kind {
			case "stopped":
				changed.State.Running = false
			case "pid":
				changed.State.Pid = 0
			case "name":
				changed.Name += "-other"
			case "label":
				changed.Config.Labels = map[string]string{}
			case "configuration":
				changed.HostConfig.Privileged = true
			case "parent":
				probes.locate = func(context.Context, string, int) (processgroup.ContainerReference, error) {
					return processgroup.ContainerReference{}, nil
				}
			case "clock":
				probes.stamp = func(time.Time) (agentchannel.Deadline, error) {
					return agentchannel.Deadline{}, errors.New("测试时钟读取失败")
				}
			}
			f.engine.containers[resource.Name] = changed
			if evidence, err := probes.check(t.Context(), resource, current); err == nil || evidence != (computerprobe.Evidence{}) || len(groups.created) != 0 {
				t.Fatal("漂移实例进入探针", evidence, err)
			}
		})
	}
}

func TestProbeOptionsAndNames(t *testing.T) {
	valid := testProbeOptions()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ProbeOptions){func(o *ProbeOptions) { o.CallTimeout = 0 }, func(o *ProbeOptions) { o.CallTimeout = time.Minute + 1 }, func(o *ProbeOptions) { o.CleanupTimeout = 0 }, func(o *ProbeOptions) { o.Groups.MaximumProcesses = 1 }, func(o *ProbeOptions) { o.Groups.GracePeriod = o.CleanupTimeout }} {
		options := valid
		mutate(&options)
		if options.Validate() == nil {
			t.Fatal("无效探针配置被接受")
		}
	}
	for _, id := range []string{"../wrong", strings.ToLower(ids.New()), ""} {
		if _, err := probeRecordName(id); err == nil {
			t.Fatal("无效用户路径被接受")
		}
	}
}

func TestNodeProbeRecoveryRejectsMalformedEntries(t *testing.T) {
	for _, kind := range []string{"name", "record", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f, probes, groups, resource, _ := newProbeFixture(t)
			name := resource.Specification.UserID + ".probe.json"
			if kind == "name" {
				name = "wrong.probe.json"
			}
			if err := f.node.journal.writeFile(name, []byte("{}")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			if err := probes.recover(ctx); err == nil {
				t.Fatal("未知恢复记录被忽略")
			}
			if len(groups.drained) != 0 {
				t.Fatal("未知恢复记录操作了资源组")
			}
			if _, err := f.node.journal.root.Lstat(name); err != nil {
				t.Fatal("恢复移除了未知记录", err)
			}
		})
	}
}
