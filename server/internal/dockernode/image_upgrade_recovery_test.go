package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// stopLabelDriftEngine 模拟停机后读取到不同的受管标签，检验删除前的复核。
type stopLabelDriftEngine struct{ *fakeEngine }

func (e stopLabelDriftEngine) stopContainer(ctx context.Context, id string) error {
	if err := e.fakeEngine.stopContainer(ctx, id); err != nil {
		return err
	}
	c, ok := e.findContainer(id)
	if !ok {
		return errMissing
	}
	c.Config.Labels = maps.Clone(c.Config.Labels)
	c.Config.Labels["io.tombot.identity"] = "changed"
	e.containers[strings.TrimPrefix(c.Name, "/")] = c
	return nil
}

func TestResumeStoppedLegacyContainerUpgrade(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	source, err := f.node.Ensure(ctx, f.specification)
	if err != nil {
		t.Fatal(err)
	}
	request := imageUpgradeRequest(f.specification, source.ContainerID, "b")
	name := resourceName(request.Current)
	old := f.engine.containers[name]
	delete(old.Config.Labels, computer.DesktopBrowserLabel)
	old.HostConfig.Mounts[1].VolumeOptions.Subpath = computer.BrowserSubdirectory
	f.engine.containers[name] = old
	f.node.engine = stopLabelDriftEngine{f.engine}
	_, err = f.node.UpgradeComputer(ctx, request)
	requireFailure(t, err, "container_stop_unconfirmed")
	if _, err := f.engine.inspectContainer(ctx, source.ContainerID); err != nil || f.storage.removals != 0 || f.engine.creates != 1 {
		t.Fatal("未核实停止状态时已删除旧容器或更换了电脑", err)
	}

	// 隔离时本地已有升级意图。恢复入口只能读取 Docker，且须核对同一份计划。
	dir, err := os.MkdirTemp("/tmp", "stop-check-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1.47/containers/"+source.ContainerID+"/json" {
			t.Error("恢复检查发出了非预期的 Docker 请求", r.Method)
			w.WriteHeader(400)
			return
		}
		c, err := f.engine.inspectContainer(r.Context(), source.ContainerID)
		if err != nil {
			t.Error(err)
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode(c); err != nil {
			t.Error(err)
		}
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	proof := UpgradeRecoveryProof{Source: request.Current, ContainerID: source.ContainerID, TargetImage: request.Target.ImageReference, TargetFence: request.Target.FencingToken, ErrorCode: "container_stop_unconfirmed"}
	check := func(p UpgradeRecoveryProof) error {
		return p.CheckSource(ctx, f.statePath, uint32(os.Geteuid()), socket, time.Second)
	}
	stale := proof
	stale.TargetFence++
	requireFailure(t, check(stale), "computer_image_upgrade_source_mismatch")
	stale = proof
	stale.TargetImage = request.Current.ImageReference
	requireFailure(t, check(stale), "computer_image_upgrade_source_mismatch")

	setSource := func(c container) {
		f.engine.mu.Lock()
		defer f.engine.mu.Unlock()
		f.engine.containers[name] = c
	}
	setSource(old)
	requireFailure(t, check(proof), "container_stop_unconfirmed")
	old.State.Status, old.State.Running = "exited", false
	old.State.Pid = 42
	setSource(old)
	requireFailure(t, check(proof), "container_stop_unconfirmed")
	old.State.Pid = 0
	setSource(old)
	if err := check(proof); err != nil {
		t.Fatal("无法恢复已经退出的原容器", err)
	}
	f.node.engine = programTestEngine{&recoveryTestEngine{fakeEngine: f.engine}}
	f.node.options.ProgramCore, f.node.options.ProgramImage = strings.Repeat("c", 64), request.Target.ImageReference
	request.Target.FencingToken++
	target, err := f.node.UpgradeComputer(ctx, request)
	if err != nil || target.State != "ready" || !target.Matches(request.Target) || !target.Capabilities.Ready() || target.ContainerID == source.ContainerID || f.engine.creates != 2 || f.storage.ensures != 1 || f.storage.removals != 0 {
		t.Fatal("原电脑未在原盘上完成拆分镜像迁移", target, err)
	}
}

func TestImageUpgradeFromInterruptedRecovery(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	source, err := f.node.Ensure(ctx, f.specification)
	if err != nil {
		t.Fatal(err)
	}
	request := imageUpgradeRequest(f.specification, source.ContainerID, "b")
	resource := f.node.resource(f.specification)
	state, err := f.node.journal.read(f.specification.UserID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.node.load(ctx, resource, state)
	if err != nil {
		t.Fatal(err)
	}
	f.probe.coreMissing, f.probe.missing = true, true
	requireFailure(t, f.node.resumeComputer(ctx, resource, state, current), "computer_probe_failed")
	for _, marker := range []string{"", strings.Repeat("f", 64), source.ContainerID} {
		if marker == "" {
			err = f.node.journal.root.Remove(state.recoveryFile())
		} else {
			err = f.node.journal.writeFile(state.recoveryFile(), []byte(marker))
		}
		if err != nil {
			t.Fatal(err)
		}
		f.engine.events = nil
		_, err = f.node.UpgradeComputer(ctx, request)
		if marker != source.ContainerID {
			requireFailure(t, err, "computer_image_upgrade_source_mismatch")
			if len(f.engine.events) != 0 {
				t.Fatal("恢复标记缺失或不匹配时更改了原容器", f.engine.events)
			}
			continue
		}
		// 旧容器可以退役；新容器仍须通过独立的能力验收才能交给会话。
		requireFailure(t, err, "computer_probe_failed")
	}
	if _, err := f.node.journal.root.Stat(state.recoveryFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧代恢复标记未随已持久化的升级意图解除", err)
	}
	f.probe.coreMissing, f.probe.missing = false, false
	target, err := f.node.UpgradeComputer(ctx, request)
	if err != nil || !target.Matches(request.Target) || target.State != "ready" || !target.Capabilities.Ready() || target.ContainerID == source.ContainerID {
		t.Fatal("恢复中断后未完成原计划的目标验收", target, err)
	}
	if f.engine.creates != 2 || f.storage.ensures != 1 || f.storage.removals != 0 {
		t.Fatal("恢复升级时重复创建了容器或改变了原盘")
	}
}
