package dockernode

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

type recoveryTestEngine struct {
	*fakeEngine
	beforeStart func(string)
	startError  error
}

func (engine *recoveryTestEngine) startContainer(ctx context.Context, id string) error {
	if engine.beforeStart != nil {
		engine.beforeStart(id)
	}
	if err := engine.fakeEngine.startContainer(ctx, id); err != nil {
		return err
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	current, _ := engine.findContainer(id)
	current.State.Status = "running"
	current.State.Pid = 4243
	engine.containers[strings.TrimPrefix(current.Name, "/")] = current
	return engine.startError
}

func simulateComputerShutdown(t *testing.T, test fixture, resource, disk Resource) {
	t.Helper()
	current, err := test.engine.inspectContainer(t.Context(), resource.Name)
	if err != nil {
		t.Fatal(err)
	}
	current.State.Running = false
	current.State.Status = "exited"
	current.State.Pid = 0
	test.engine.containers[resource.Name] = current
	workspace := test.storage.workspaces[disk.Name]
	workspace.Ready = false
	test.storage.workspaces[disk.Name] = workspace
	test.engine.events = nil
}

func TestNodeRecoversStoppedComputerAfterClosingOldSession(t *testing.T) {
	ctx := t.Context()
	test := newFixture(t)
	oldSession := test.prepareComputer(t, test.specification)
	groups := &fakeGroups{path: t.TempDir()}
	group, err := groups.Locate(ctx, oldSession, 4242)
	if err != nil {
		t.Fatal(err)
	}
	oldCommand := commandFor(oldSession)
	oldState := &sessionRecord{Version: 2, Command: oldCommand, Epoch: ids.New(), Phase: "active", Deadline: time.Now().Add(time.Hour), Group: &group}
	if err := test.node.journal.saveSession(oldState); err != nil {
		t.Fatal(err)
	}
	resource := test.node.resource(test.specification)
	state, err := test.node.journal.read(test.specification.UserID)
	if err != nil || state == nil {
		t.Fatal("缺少电脑原始记录", err)
	}
	state.Specification.LeaseExpiresAt = time.Now().Add(-time.Hour).UTC()
	if err := test.node.journal.save(state); err != nil {
		t.Fatal(err)
	}
	simulateComputerShutdown(t, test, resource, resource)
	var agentPrepared bool
	test.node.agent = &fakeAgentAvailability{prepare: func(_ context.Context, _ Resource, state *record) error {
		agentPrepared = true
		if state.Phase != "creating" || state.ContainerID != oldSession.ContainerID {
			t.Fatal("封闭代理授权前未写入正确的恢复状态")
		}
		return nil
	}}
	test.node.engine = &recoveryTestEngine{fakeEngine: test.engine, beforeStart: func(id string) {
		closed, err := test.node.journal.readSession(sessionKeyOf(oldSession))
		if err != nil || closed == nil || closed.Phase != "closed" || groups.drains != 1 {
			t.Fatal("旧执行尚未关闭即启动容器", err)
		}
		pending, err := test.node.journal.read(resource.Specification.UserID)
		if err != nil || pending == nil || pending.Phase != "creating" || pending.ContainerID != oldSession.ContainerID || id != oldSession.ContainerID {
			t.Fatal("启动容器前未持久封闭就绪状态", err)
		}
		if !agentPrepared || !test.storage.workspaces[resource.Name].Ready {
			t.Fatal("代理尚未准备或原磁盘未挂载即启动容器")
		}
	}}
	controller := test.sessionController(t, groups, sessionOptions())
	restored, err := test.node.journal.read(test.specification.UserID)
	if err != nil || restored == nil || restored.Phase != "ready" || restored.ContainerID != oldSession.ContainerID || !restored.Specification.LeaseExpiresAt.Equal(state.Specification.LeaseExpiresAt) {
		t.Fatalf("原电脑未就绪或旧租约发生变更：%+v，%v", restored, err)
	}
	if !slices.Equal(test.engine.events, []string{"start-container"}) || test.storage.ensures != 1 || test.storage.restores != 1 || test.storage.removals != 0 {
		t.Fatal("启动恢复改变了原容器或持久磁盘", test.engine.events)
	}
	if _, err := test.node.journal.root.Stat(".recover-" + test.specification.UserID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("电脑恢复完成后仍保留恢复标记", err)
	}
	_, err = controller.Open(ctx, oldCommand)
	requireFailure(t, err, "session_cannot_reopen")
	newSession := oldSession
	newSession.SessionID = ids.New()
	newSession.RunID = ids.New()
	newSession.FencingToken++
	newCommand := commandFor(newSession)
	if observation, err := controller.Open(ctx, newCommand); err != nil || !observation.Matches(newCommand, "active") {
		t.Fatal("恢复后无法在原容器接受新会话", observation, err)
	}
}

func TestNodeRecoversStoppedComputerOnRetainedDisk(t *testing.T) {
	ctx := t.Context()
	test := newFixture(t)
	oldComputer, err := test.node.Ensure(ctx, test.specification)
	if err != nil {
		t.Fatal(err)
	}
	request := imageUpgradeRequest(test.specification, oldComputer.ContainerID, "b")
	upgraded, err := test.node.UpgradeComputer(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	resource := test.node.resource(request.Target)
	disk := test.node.resource(request.Current)
	simulateComputerShutdown(t, test, resource, disk)
	test.node.engine = &recoveryTestEngine{fakeEngine: test.engine}
	test.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
	state, err := test.node.journal.read(request.Target.UserID)
	if err != nil || state == nil || state.Phase != "ready" || state.ContainerID != upgraded.ContainerID || state.StorageGeneration != request.Current.Generation {
		t.Fatalf("恢复改变了镜像升级后的容器或磁盘身份：%+v，%v", state, err)
	}
	if !slices.Equal(test.engine.events, []string{"start-container"}) || test.storage.ensures != 1 || test.storage.restores != 1 || test.storage.removals != 0 || len(test.storage.workspaces) != 1 || !test.storage.workspaces[disk.Name].Ready {
		t.Fatal("镜像升级后的电脑未使用原磁盘恢复", test.engine.events)
	}
}

func TestNodeKeepsFailedRecoveryClosedUntilProbePasses(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		startError   error
		probeMissing bool
	}{
		{name: "探针失败", probeMissing: true},
		{name: "Docker 启动回包丢失", startError: context.DeadlineExceeded},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			verifyFailedComputerRecovery(t, scenario.startError, scenario.probeMissing)
		})
	}
}

func verifyFailedComputerRecovery(t *testing.T, startError error, probeMissing bool) {
	t.Helper()
	ctx := t.Context()
	test := newFixture(t)
	recovering := test.prepareComputer(t, test.specification)
	other := test.specification
	other.UserID = ids.New()
	other.ComputerID = ids.New()
	other.OperationID = ids.New()
	other.VolumeReference = "tombot-user-" + other.UserID
	healthy := test.prepareComputer(t, other)
	resource := test.node.resource(test.specification)
	simulateComputerShutdown(t, test, resource, resource)
	engine := &recoveryTestEngine{fakeEngine: test.engine, startError: startError}
	test.node.engine = engine
	test.probe.missing = probeMissing
	var closedEgress []string
	test.node.egress = &fakeEgress{stopCall: func(_ context.Context, resource Resource) error {
		closedEgress = append(closedEgress, resource.Specification.UserID)
		return nil
	}}
	groups := &fakeGroups{path: t.TempDir()}
	first := test.sessionController(t, groups, sessionOptions())
	state, err := test.node.journal.read(recovering.UserID)
	if err != nil || state == nil || state.Phase != "creating" || state.ContainerID != recovering.ContainerID || !slices.Equal(closedEgress, []string{recovering.UserID}) {
		t.Fatalf("恢复失败未隔离原电脑或出网：%+v，%v", state, err)
	}
	marker, err := test.node.journal.readFile(".recover-" + recovering.UserID)
	if err != nil || string(marker) != recovering.ContainerID {
		t.Fatal("恢复失败未保留可继续恢复的原容器身份", err)
	}
	_, err = first.Open(ctx, commandFor(recovering))
	requireFailure(t, err, "session_computer_mismatch")
	healthyCommand := commandFor(healthy)
	if observation, err := first.Open(ctx, healthyCommand); err != nil || !observation.Matches(healthyCommand, "active") {
		t.Fatal("单用户恢复失败影响其他电脑", observation, err)
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	engine.startError = nil
	test.probe.missing = false
	second := test.sessionController(t, groups, sessionOptions())
	command := commandFor(recovering)
	if observation, err := second.Open(ctx, command); err != nil || !observation.Matches(command, "active") {
		t.Fatal("原容器复验通过后仍无法开启会话", observation, err)
	}
	if !slices.Equal(test.engine.events, []string{"start-container"}) || test.storage.restores != 1 {
		t.Fatal("重试时重复操作已运行的原容器或已挂载的磁盘", test.engine.events)
	}
}

func TestNodeRechecksReadyComputerWithPendingRecovery(t *testing.T) {
	ctx := t.Context()
	test := newFixture(t)
	session := test.prepareComputer(t, test.specification)
	groups := &fakeGroups{path: t.TempDir()}
	first := test.sessionController(t, groups, sessionOptions())
	marker := ".recover-" + session.UserID
	// 就绪记录的替换可能已经可见，但目录同步仍未完成，恢复意图必须继续阻挡新请求。
	if err := test.node.journal.writeFile(marker, []byte(session.ContainerID)); err != nil {
		t.Fatal(err)
	}
	command := commandFor(session)
	_, err := first.Open(ctx, command)
	requireFailure(t, err, "session_computer_recovering")
	if err := first.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	test.engine.events = nil
	test.probe.missing = true
	second := test.sessionController(t, groups, sessionOptions())
	state, err := test.node.journal.read(session.UserID)
	if err != nil || state == nil || state.Phase != "creating" || state.ContainerID != session.ContainerID || len(test.engine.events) != 0 {
		t.Fatalf("启动时未在原容器复核可见的就绪记录：%+v，%v", state, err)
	}
	if err := second.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	test.probe.missing = false
	third := test.sessionController(t, groups, sessionOptions())
	if _, err := test.node.journal.root.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("复验通过的原容器仍被恢复意图阻挡", err)
	}
	if observation, err := third.Open(ctx, command); err != nil || !observation.Matches(command, "active") || len(test.engine.events) != 0 || test.storage.restores != 0 {
		t.Fatal("已运行的原容器复验通过后无法接受新会话或被重复操作", observation, err)
	}
}

func TestNodeRecoveryRequiresOriginalContainer(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		replace bool
	}{
		{name: "原容器缺失"},
		{name: "同名容器身份变化", replace: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			test := newFixture(t)
			session := test.prepareComputer(t, test.specification)
			resource := test.node.resource(test.specification)
			simulateComputerShutdown(t, test, resource, resource)
			if scenario.replace {
				current := test.engine.containers[resource.Name]
				current.ID = strings.Repeat("e", 64)
				test.engine.containers[resource.Name] = current
			} else {
				delete(test.engine.containers, resource.Name)
			}
			test.node.engine = &recoveryTestEngine{fakeEngine: test.engine}
			controller := test.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
			if _, err := controller.Open(t.Context(), commandFor(session)); err == nil {
				t.Fatal("原容器身份无法确认时开放了新会话")
			}
			if len(test.engine.events) != 0 || test.storage.ensures != 1 || test.storage.restores != 0 || test.storage.removals != 0 {
				t.Fatal("恢复重新分配了容器或操作了原磁盘", test.engine.events)
			}
		})
	}
}

func TestNodeRecoverySkipsUnfinishedLifecycle(t *testing.T) {
	for _, phase := range []string{"creating", "upgrading"} {
		t.Run(phase, func(t *testing.T) {
			ctx := t.Context()
			test := newFixture(t)
			observation, err := test.node.Ensure(ctx, test.specification)
			if err != nil {
				t.Fatal(err)
			}
			state, err := test.node.journal.read(test.specification.UserID)
			if err != nil || state == nil {
				t.Fatal("缺少电脑记录", err)
			}
			state.Phase = phase
			if phase == "upgrading" {
				request := imageUpgradeRequest(test.specification, observation.ContainerID, "b")
				state.ImageUpgrade = &request
				state.StorageGeneration = test.specification.Generation
			}
			if err := test.node.journal.save(state); err != nil {
				t.Fatal(err)
			}
			resource := test.node.resource(test.specification)
			simulateComputerShutdown(t, test, resource, resource)
			test.node.engine = &recoveryTestEngine{fakeEngine: test.engine}
			test.sessionController(t, &fakeGroups{path: t.TempDir()}, sessionOptions())
			pending, err := test.node.journal.read(test.specification.UserID)
			if err != nil || pending == nil || pending.Phase != phase || len(test.engine.events) != 0 || test.storage.restores != 0 {
				t.Fatalf("启动恢复改变了尚未完成的电脑：%+v，%v", pending, err)
			}
		})
	}
}
