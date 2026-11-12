package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/computerprobe"
	"io.yueyang/tom/internal/egressproxy"
	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/infra/ids"
)

// fakeEngine 用于控制结果丢失及资源漂移；真实 Docker 验证放在独立集成测试中。
type fakeEngine struct {
	mu                sync.Mutex
	volumes           map[string]volume
	containers        map[string]container
	events            []string
	creates           int
	lostCreate        bool
	stopError         error
	removeVolumeError error
	imageError        error
}

func (engine *fakeEngine) inspectImage(_ context.Context, reference string) (imageDescription, error) {
	return testImage(reference, "linux/"+runtime.GOARCH), engine.imageError
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{volumes: map[string]volume{}, containers: map[string]container{}}
}
func (engine *fakeEngine) inspectVolume(_ context.Context, name string) (volume, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value, ok := engine.volumes[name]
	if !ok {
		return volume{}, errMissing
	}
	return value, nil
}
func (engine *fakeEngine) createVolume(_ context.Context, name, path string, labels map[string]string) (volume, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value := volume{Name: name, Driver: "local", Labels: labels, Options: map[string]string{"type": "none", "device": path, "o": "bind"}}
	engine.volumes[name] = value
	engine.events = append(engine.events, "create-volume")
	return value, nil
}
func (engine *fakeEngine) removeVolume(_ context.Context, name string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.removeVolumeError != nil {
		return engine.removeVolumeError
	}
	delete(engine.volumes, name)
	engine.events = append(engine.events, "remove-volume")
	return nil
}
func (engine *fakeEngine) findContainer(id string) (container, bool) {
	value, ok := engine.containers[id]
	if ok {
		return value, true
	}
	for _, value := range engine.containers {
		if value.ID == id {
			return value, true
		}
	}
	return container{}, false
}
func (engine *fakeEngine) inspectContainer(_ context.Context, id string) (container, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value, ok := engine.findContainer(id)
	if !ok {
		return container{}, errMissing
	}
	return value, nil
}
func (engine *fakeEngine) createContainer(_ context.Context, name string, configuration createContainer) (string, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value := container{ID: secure.Hash(name), Name: "/" + name, HostConfig: configuration.HostConfig}
	value.Config.Image = configuration.Image
	value.Config.User = configuration.User
	value.Config.Entrypoint = configuration.Entrypoint
	value.Config.Cmd = configuration.Cmd
	value.Config.Labels = configuration.Labels
	for _, item := range configuration.HostConfig.Mounts {
		mounted := mountedResource{Type: item.Type, Source: item.Source, Destination: item.Target, RW: !item.ReadOnly}
		if item.Type == "volume" {
			mounted.Name = item.Source
		}
		value.Mounts = append(value.Mounts, mounted)
	}
	value.NetworkSettings.Networks = map[string]networkEndpoint{"none": {}}
	engine.containers[name] = value
	engine.creates++
	engine.events = append(engine.events, "create-container")
	if engine.lostCreate {
		return "", context.DeadlineExceeded
	}
	return value.ID, nil
}
func (engine *fakeEngine) startContainer(_ context.Context, id string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value, ok := engine.findContainer(id)
	if !ok {
		return errMissing
	}
	value.State.Running = true
	engine.containers[strings.TrimPrefix(value.Name, "/")] = value
	engine.events = append(engine.events, "start-container")
	return nil
}
func (engine *fakeEngine) stopContainer(_ context.Context, id string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.stopError != nil {
		return engine.stopError
	}
	value, ok := engine.findContainer(id)
	if !ok {
		return errMissing
	}
	value.State.Running = false
	engine.containers[strings.TrimPrefix(value.Name, "/")] = value
	engine.events = append(engine.events, "stop-container")
	return nil
}
func (engine *fakeEngine) removeContainer(_ context.Context, id string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	value, ok := engine.findContainer(id)
	if !ok {
		return errMissing
	}
	if value.State.Running {
		return errors.New("测试拒绝删除运行容器")
	}
	delete(engine.containers, strings.TrimPrefix(value.Name, "/"))
	engine.events = append(engine.events, "remove-container")
	return nil
}

type fakeStorage struct {
	mu          sync.Mutex
	workspaces  map[string]Workspace
	removals    int
	verifyError error
}

func (storage *fakeStorage) VerifyFilesystem(context.Context, Resource, int64, uint64) error {
	return storage.verifyError
}

func (storage *fakeStorage) Inspect(_ context.Context, resource Resource) (Workspace, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return storage.workspaces[resource.Name], nil
}
func (storage *fakeStorage) Ensure(_ context.Context, resource Resource) (Workspace, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	value := Workspace{Exists: true, Ready: true, Path: resource.WorkspacePath, CapacityBytes: resource.Specification.Resources.DiskBytes}
	storage.workspaces[resource.Name] = value
	return value, nil
}
func (storage *fakeStorage) Remove(_ context.Context, resource Resource) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	delete(storage.workspaces, resource.Name)
	storage.removals++
	return nil
}

type fakeProbe struct{ missing bool }

func (probe *fakeProbe) Check(context.Context, Resource, string, computerprobe.Evidence) (computer.Capabilities, error) {
	return computer.Capabilities{Terminal: true, Browser: !probe.missing, Files: true, ProcessReaping: true, DiskQuota: true, NetworkIsolation: true}, nil
}

type fixture struct {
	node          *Runtime
	engine        *fakeEngine
	storage       *fakeStorage
	probe         *fakeProbe
	specification computer.Specification
	statePath     string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	statePath := t.TempDir()
	journal, err := openJournal(statePath, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	userID, nodeID := ids.New(), ids.New()
	specification := computer.Specification{OperationID: ids.New(), UserID: userID, ComputerID: ids.New(), NodeID: nodeID, ImageReference: "registry.example.test/computer@sha256:" + strings.Repeat("a", 64), VolumeReference: "tombot-user-" + userID,
		Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, ProcessLimit: 128}}
	engine := newFakeEngine()
	storage := &fakeStorage{workspaces: map[string]Workspace{}}
	probe := &fakeProbe{}
	node := &Runtime{options: Options{NodeID: nodeID, StorageDirectory: t.TempDir(), StateDirectory: statePath, Socket: "/run/docker.sock", CallTimeout: time.Second, MaximumLease: 2 * time.Minute, TemporaryBytes: 64 << 20, RuntimeBytes: 128 << 20, SharedMemoryBytes: 256 << 20, Agent: testAgentOptions(), Egress: testEgressOptions(), MaximumEgressComputers: 8}, engine: engine, storage: storage, probe: probe, journal: journal, closeEngine: func() {}, egress: &fakeEgress{}}
	node.agent = &fakeAgentAvailability{}
	node.options.AgentStartup = testAgentStartupOptions()
	node.options.Probes = testProbeOptions()
	node.options.Browser = testBrowserOptions()
	node.browsers = &fakeComputerBrowsers{}
	node.mounts = func() ([]filesystemMount, error) { return nil, nil }
	node.probes = &fakeComputerProbes{}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture{node, engine, storage, probe, specification, statePath}
}

func testEgressOptions() egressproxy.Options {
	return egressproxy.Options{Ports: []uint16{80, 443}, ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: time.Second, RequestTimeout: 3 * time.Second, TunnelTimeout: 3 * time.Second, ShutdownTimeout: 2 * time.Second, MaximumConnections: 8, MaximumRequestBytes: 4096, MaximumResponseBytes: 4096, MaximumTunnelBytes: 8192}
}

func testAgentOptions() computeragent.RuntimeOptions {
	return computeragent.RuntimeOptions{EgressConnectTimeoutMS: 1000, EgressIdleTimeoutMS: 1000, EgressLifetimeMS: 10000, EgressMaximumConnections: 8, EgressMaximumTransferBytes: 1 << 20, MaximumActive: 4, CallTimeoutMS: 1000, CleanupTimeoutMS: 1000, RetryDelayMS: 10, CleanupAttempts: 2, PollIntervalMS: 10, ShutdownTimeoutMS: 3000, MaximumConnections: 8}
}
func requireFailure(t *testing.T, err error, code string) {
	t.Helper()
	var failure *computer.Failure
	if !errors.As(err, &failure) || !failure.IsPermanent || failure.Code != code {
		t.Fatalf("节点错误分类不符：%v，预期 %s", err, code)
	}
}

func TestLifecycleIdentityFencingAndCleanup(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	specification := fixture.specification
	observation, err := fixture.node.Inspect(ctx, specification)
	if err != nil || observation.State != "absent" {
		t.Fatal("初始对账错误", err)
	}
	observation, err = fixture.node.Ensure(ctx, specification)
	if err != nil || observation.State != "ready" || !observation.Capabilities.Ready() {
		t.Fatal("未就绪", err)
	}
	if _, err = fixture.node.Ensure(ctx, specification); err != nil {
		t.Fatal(err)
	}
	if fixture.engine.creates != 1 {
		t.Fatal("重复请求创建第二个容器")
	}
	journal, err := openJournal(fixture.statePath, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := journal.root.Close(); err != nil {
			t.Error(err)
		}
	}()
	restarted := *fixture.node
	restarted.journal = journal
	next := specification
	next.FencingToken++
	if _, err = restarted.Inspect(ctx, next); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.node.Remove(ctx, specification)
	requireFailure(t, err, "stale_controller")
	newGeneration := next
	newGeneration.Generation++
	newGeneration.Attempt++
	newGeneration.FencingToken++
	_, err = restarted.Ensure(ctx, newGeneration)
	requireFailure(t, err, "generation_not_released")
	observation, err = restarted.Remove(ctx, next)
	if err != nil || observation.State != "absent" {
		t.Fatal("未确认清理", err)
	}
	_, err = restarted.Ensure(ctx, next)
	requireFailure(t, err, "generation_closed")
	observation, err = restarted.Ensure(ctx, newGeneration)
	if err != nil || observation.State != "ready" {
		t.Fatal("新代次不能创建", err)
	}
	if fixture.node.resource(next).Name == fixture.node.resource(newGeneration).Name || fixture.node.resource(next).WorkspacePath == fixture.node.resource(newGeneration).WorkspacePath {
		t.Fatal("不同代次复用了物理资源名称")
	}
	_, err = restarted.Remove(ctx, next)
	requireFailure(t, err, "generation_not_released")
	if fixture.storage.removals != 1 || fixture.engine.creates != 2 {
		t.Fatal("迟到清理影响了新实例")
	}
	want := []string{"stop-container", "remove-container", "remove-volume"}
	actual := fixture.engine.events[3:6]
	for index, event := range want {
		if actual[index] != event {
			t.Fatal("清理顺序错误", actual)
		}
	}
}

func TestUnknownResponsesAndQuarantine(t *testing.T) {
	ctx := context.Background()
	t.Run("创建回包丢失仍复用原容器", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.engine.lostCreate = true
		if _, err := fixture.node.Ensure(ctx, fixture.specification); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		fixture.engine.lostCreate = false
		observation, err := fixture.node.Ensure(ctx, fixture.specification)
		if err != nil || observation.State != "ready" {
			t.Fatal(err)
		}
		if fixture.engine.creates != 1 {
			t.Fatal("未知结果导致重建")
		}
	})
	t.Run("停止结果未知不能删除卷且同代次禁止再创建", func(t *testing.T) {
		fixture := newFixture(t)
		if _, err := fixture.node.Ensure(ctx, fixture.specification); err != nil {
			t.Fatal(err)
		}
		fixture.engine.stopError = context.DeadlineExceeded
		if _, err := fixture.node.Remove(ctx, fixture.specification); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if fixture.storage.removals != 0 || len(fixture.engine.volumes) != 1 {
			t.Fatal("未停止就删除存储")
		}
		_, err := fixture.node.Ensure(ctx, fixture.specification)
		requireFailure(t, err, "generation_closed")
		fixture.engine.stopError = nil
		if _, err = fixture.node.Remove(ctx, fixture.specification); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("卷仍被占用不能删除持久文件系统", func(t *testing.T) {
		fixture := newFixture(t)
		if _, err := fixture.node.Ensure(ctx, fixture.specification); err != nil {
			t.Fatal(err)
		}
		fixture.engine.removeVolumeError = &engineError{409}
		if _, err := fixture.node.Remove(ctx, fixture.specification); err == nil {
			t.Fatal("卷删除失败被忽略")
		}
		if fixture.storage.removals != 0 {
			t.Fatal("占用卷的存储被删除")
		}
		fixture.engine.removeVolumeError = nil
		if _, err := fixture.node.Remove(ctx, fixture.specification); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("真实能力缺失不得标为就绪", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.probe.missing = true
		_, err := fixture.node.Ensure(ctx, fixture.specification)
		requireFailure(t, err, "computer_probe_failed")
		observation, err := fixture.node.Inspect(ctx, fixture.specification)
		if err != nil || observation.State == "ready" {
			t.Fatal("缺少浏览器仍就绪", err)
		}
	})
}

func TestDriftAndInvalidClaims(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*fixture)
		code   string
	}{
		{"错误卷路径", func(f *fixture) {
			name := f.node.resource(f.specification).Name
			value := f.engine.volumes[name]
			value.Options = map[string]string{"type": "none", "device": "/etc", "o": "bind"}
			f.engine.volumes[name] = value
		}, "volume_identity_mismatch"},
		{"特权容器", func(f *fixture) {
			name := f.node.resource(f.specification).Name
			value := f.engine.containers[name]
			value.HostConfig.Privileged = true
			f.engine.containers[name] = value
		}, "container_isolation_mismatch"},
		{"错误用户标签", func(f *fixture) {
			name := f.node.resource(f.specification).Name
			value := f.engine.containers[name]
			value.Config.Labels = map[string]string{"io.tombot.user": ids.New()}
			f.engine.containers[name] = value
		}, "container_identity_mismatch"},
		{"额外挂载桥接网络", func(f *fixture) {
			name := f.node.resource(f.specification).Name
			value := f.engine.containers[name]
			value.NetworkSettings.Networks["bridge"] = networkEndpoint{IPAddress: "172.17.0.2"}
			f.engine.containers[name] = value
		}, "container_network_mismatch"},
		{"共享宿主网络", func(f *fixture) {
			name := f.node.resource(f.specification).Name
			value := f.engine.containers[name]
			value.HostConfig.NetworkMode = "host"
			f.engine.containers[name] = value
		}, "container_isolation_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			if _, err := fixture.node.Ensure(ctx, fixture.specification); err != nil {
				t.Fatal(err)
			}
			test.mutate(&fixture)
			before := len(fixture.engine.events)
			_, err := fixture.node.Remove(ctx, fixture.specification)
			requireFailure(t, err, test.code)
			if len(fixture.engine.events) != before || fixture.storage.removals != 0 {
				t.Fatal("漂移资源仍被修改")
			}
		})
	}
	fixture := newFixture(t)
	if _, err := fixture.node.Inspect(ctx, fixture.specification); err != nil {
		t.Fatal(err)
	}
	changed := fixture.specification
	changed.Resources.CPUMillis++
	_, err := fixture.node.Ensure(ctx, changed)
	requireFailure(t, err, "stale_controller")
	changed = fixture.specification
	changed.LeaseExpiresAt = changed.LeaseExpiresAt.Add(time.Second)
	_, err = fixture.node.Ensure(ctx, changed)
	requireFailure(t, err, "stale_controller")
	changed = fixture.specification
	changed.LeaseExpiresAt = time.Now().Add(-time.Second)
	_, err = fixture.node.Ensure(ctx, changed)
	requireFailure(t, err, "lease_invalid")
	changed = fixture.specification
	changed.NodeID = ids.New()
	_, err = fixture.node.Ensure(ctx, changed)
	requireFailure(t, err, "specification_invalid")
}

func TestDurableLockAndCorruptState(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	results := make(chan error, 8)
	for range 8 {
		go func() { _, err := fixture.node.Ensure(ctx, fixture.specification); results <- err }()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		return
	}
	if fixture.engine.creates != 1 {
		t.Fatal("并发创建重复实例")
	}
	release, err := fixture.node.journal.lock(ctx, fixture.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = fixture.node.Inspect(short, fixture.specification)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("资源锁等待不受时限控制", err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.statePath, fixture.specification.UserID+".json")
	if err = os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := len(fixture.engine.events)
	if _, err = fixture.node.Remove(ctx, fixture.specification); err == nil {
		t.Fatal("损坏记录被当作新资源")
	}
	if len(fixture.engine.events) != before {
		t.Fatal("损坏记录触发删除")
	}
}

func TestContainerRequiresReadyStorageMount(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	if _, err := fixture.node.Ensure(ctx, fixture.specification); err != nil {
		t.Fatal(err)
	}
	resource := fixture.node.resource(fixture.specification)
	workspace := fixture.storage.workspaces[resource.Name]
	workspace.Ready = false
	fixture.storage.workspaces[resource.Name] = workspace
	_, err := fixture.node.Inspect(ctx, fixture.specification)
	requireFailure(t, err, "container_dependencies_missing")
	container := fixture.engine.containers[resource.Name]
	container.State.Running = false
	fixture.engine.containers[resource.Name] = container
	observation, err := fixture.node.Inspect(ctx, fixture.specification)
	if err != nil || observation.State == "ready" {
		t.Fatal("尚未恢复挂载就返回电脑就绪", err)
	}
	observation, err = fixture.node.Ensure(ctx, fixture.specification)
	if err != nil || observation.State != "ready" || !fixture.storage.workspaces[resource.Name].Ready {
		t.Fatal("容器启动前未恢复存储挂载", err)
	}
	if fixture.engine.creates != 1 {
		t.Fatal("恢复存储重新创建了容器")
	}
}

func TestLegacyNetworkJournalIsRejected(t *testing.T) {
	fixture := newFixture(t)
	if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.node.journal.read(fixture.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	state.Version = 1
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.statePath, fixture.specification.UserID+".json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	before := len(fixture.engine.events)
	for _, operation := range []func(context.Context, computer.Specification) (computer.Observation, error){fixture.node.Inspect, fixture.node.Ensure, fixture.node.Remove} {
		if _, err := operation(t.Context(), fixture.specification); err == nil {
			t.Fatal("旧桥接网络记录被接管")
		}
	}
	retained, err := os.ReadFile(path)
	if err != nil || string(retained) != string(encoded) || len(fixture.engine.events) != before || fixture.storage.removals != 0 {
		t.Fatal("拒绝旧记录时修改了资源或覆盖记录", err)
	}
}

func TestNetworkNoneRejectsRoutableEndpoints(t *testing.T) {
	for _, test := range []struct {
		name      string
		endpoints map[string]networkEndpoint
	}{
		{"缺少网络声明", nil},
		{"替换桥接", map[string]networkEndpoint{"bridge": {}}},
		{"额外接口", map[string]networkEndpoint{"none": {}, "bridge": {}}},
		{"IPv4 地址", map[string]networkEndpoint{"none": {IPAddress: "172.17.0.2"}}},
		{"IPv6 地址", map[string]networkEndpoint{"none": {GlobalIPv6Address: "2001:db8::2"}}},
		{"IPv4 网关", map[string]networkEndpoint{"none": {Gateway: "172.17.0.1"}}},
		{"IPv6 网关", map[string]networkEndpoint{"none": {IPv6Gateway: "2001:db8::1"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			if _, err := fixture.node.Ensure(t.Context(), fixture.specification); err != nil {
				t.Fatal(err)
			}
			name := fixture.node.resource(fixture.specification).Name
			value := fixture.engine.containers[name]
			value.NetworkSettings.Networks = test.endpoints
			fixture.engine.containers[name] = value
			before := len(fixture.engine.events)
			for _, operation := range []func(context.Context, computer.Specification) (computer.Observation, error){fixture.node.Inspect, fixture.node.Ensure, fixture.node.Remove} {
				_, err := operation(t.Context(), fixture.specification)
				requireFailure(t, err, "container_network_mismatch")
			}
			if len(fixture.engine.events) != before || fixture.storage.removals != 0 {
				t.Fatal("网络漂移仍操作了电脑资源")
			}
		})
	}
}
