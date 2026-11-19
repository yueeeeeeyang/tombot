package dockernode

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

type recoveryAgentStore struct {
	desktop computeragent.Snapshot
	browser computeragent.BrowserSnapshot
	probe   computeragent.ProbeSnapshot
}

func (store *recoveryAgentStore) Desktop(id string) (computeragent.Store, error) {
	if store.desktop.Command.Session.DesktopID != id {
		return nil, errors.New("测试桌面不匹配")
	}
	return store, nil
}

func (store *recoveryAgentStore) Visit(_ context.Context, visit func(computeragent.Snapshot) error) error {
	return visit(store.desktop)
}

func (store *recoveryAgentStore) Load(context.Context) (*computeragent.Snapshot, error) {
	snapshot := store.desktop
	return &snapshot, nil
}

func (store *recoveryAgentStore) Save(_ context.Context, snapshot computeragent.Snapshot) error {
	store.desktop = snapshot
	return nil
}

func (store *recoveryAgentStore) LoadBrowser(context.Context) (*computeragent.BrowserSnapshot, error) {
	snapshot := store.browser
	return &snapshot, nil
}

func (store *recoveryAgentStore) SaveBrowser(_ context.Context, snapshot computeragent.BrowserSnapshot) error {
	store.browser = snapshot
	return nil
}

func (store *recoveryAgentStore) LoadProbe(context.Context) (*computeragent.ProbeSnapshot, error) {
	snapshot := store.probe
	return &snapshot, nil
}

func (store *recoveryAgentStore) SaveProbe(_ context.Context, snapshot computeragent.ProbeSnapshot) error {
	store.probe = snapshot
	return nil
}

func newRecoveryAgentFixture(t *testing.T) (fixture, Resource, string, recoveryAgentStore) {
	t.Helper()
	test := newFixture(t)
	resource := test.node.resource(test.specification)
	session := test.prepareComputer(t, test.specification)
	groups := &fakeGroups{}
	group, err := groups.Locate(t.Context(), session, 4242)
	if err != nil {
		t.Fatal(err)
	}
	identity := test.node.agentConfiguration(resource).Identity
	parent := processgroup.ContainerReference{ContainerID: session.ContainerID, Path: group.ParentPath, Device: group.ParentDevice, Inode: group.ParentInode}
	deadline := agentchannel.Deadline{ExpiresNS: 1, TimeNamespace: 2}
	browserGroup := processgroup.ServiceReference{Container: parent, ID: ids.New(), Kind: processgroup.BrowserService}
	probeGroup := processgroup.ServiceReference{Container: parent, ID: ids.New(), Kind: processgroup.ProbeService}
	browserGrant := computeragent.BrowserGrant{Identity: identity, Revision: 1, Request: taskprocess.BrowserRequest{Version: 1, Group: browserGroup, Deadline: deadline}, Limits: testBrowserOptions().Limits}
	probeGrant := computeragent.ProbeGrant{Identity: identity, Revision: 1, Request: taskprocess.ProbeRequest{Version: 1, Group: probeGroup, Deadline: deadline}}
	store := recoveryAgentStore{
		desktop: computeragent.Snapshot{Version: 2, Command: commandFor(session), Group: &group, Phase: "active"},
		browser: computeragent.BrowserSnapshot{Version: 1, Grant: browserGrant, Phase: "consumed", StartupError: "browser_start_membership_failed"},
		probe:   computeragent.ProbeSnapshot{Version: 1, Grant: probeGrant, Phase: "consumed"},
	}
	closedDesktop := &sessionRecord{Version: 2, Command: store.desktop.Command, Epoch: ids.New(), Phase: "closed", Deadline: time.Now().UTC(), Group: &group}
	closedDesktop.Command.Revision++
	if err := test.node.journal.saveSession(closedDesktop); err != nil {
		t.Fatal(err)
	}
	// 宿主可以已封闭更新的版本，落后的代理仍须保留自己最后消费的原授权。
	for revision := int64(1); revision <= 2; revision++ {
		browserGrant.Revision = revision
		if revision > 1 {
			browserGrant.Request.Group.ID = ids.New()
		}
		for _, phase := range []string{"prepared", "closing", "closed"} {
			hostBrowser := &browserRecord{Version: 1, Specification: test.specification, Grant: browserGrant, Phase: phase}
			if phase == "closed" {
				hostBrowser.Closure = "isolated"
			}
			if err := test.node.journal.saveBrowser(hostBrowser); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, phase := range []string{"prepared", "closing", "closed"} {
		hostProbe := &probeRecord{Version: 1, Specification: test.specification, Grant: probeGrant, Phase: phase}
		if err := test.node.journal.saveProbe(hostProbe); err != nil {
			t.Fatal(err)
		}
	}
	return test, resource, session.ContainerID, store
}

func TestNodeClosesStoppedAgentWithoutReplacingFences(t *testing.T) {
	test, resource, containerID, store := newRecoveryAgentFixture(t)
	want := store
	want.desktop.Phase = "closed"
	want.browser.Phase = "closed"
	want.probe.Phase = "closed"
	for range 2 {
		if err := test.node.closeStoppedAgent(t.Context(), resource, containerID, &store); err != nil {
			t.Fatal(err)
		}
		if store != want {
			t.Fatal("恢复未封闭全部代理授权，或变更了原版本、归属或诊断")
		}
	}
}

func TestNodeRejectsStoppedAgentWithoutMatchingClosure(t *testing.T) {
	for _, scenario := range []string{"桌面关闭证据缺失", "浏览器关闭证据缺失", "探针同版本授权不符"} {
		t.Run(scenario, func(t *testing.T) {
			test, resource, containerID, store := newRecoveryAgentFixture(t)
			switch scenario {
			case "桌面关闭证据缺失":
				key := sessionKeyOf(store.desktop.Command.Session)
				if err := test.node.journal.root.Remove(key.name() + ".json"); err != nil {
					t.Fatal(err)
				}
			case "浏览器关闭证据缺失":
				name, err := browserRecordName(resource.Specification.UserID)
				if err != nil {
					t.Fatal(err)
				}
				if err := test.node.journal.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "探针同版本授权不符":
				store.probe.Grant.Request.Group.ID = ids.New()
			}
			if err := test.node.closeStoppedAgent(t.Context(), resource, containerID, &store); err == nil {
				t.Fatal("未获得正确宿主关闭证据仍确认代理回收")
			}
			switch scenario {
			case "桌面关闭证据缺失":
				if store.desktop.Phase != "active" {
					t.Fatal("桌面关闭未经确认却被覆盖")
				}
			case "浏览器关闭证据缺失":
				if store.browser.Phase != "consumed" {
					t.Fatal("浏览器关闭未经确认却被覆盖")
				}
			case "探针同版本授权不符":
				if store.probe.Phase != "consumed" {
					t.Fatal("探针关闭未经确认却被覆盖")
				}
			}
		})
	}
}

func TestNodeClosesStoppedAgentRecordsOnDisk(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || checkProtectedParents(os.TempDir()) != nil {
		t.Skip("需要 Linux root 和受保护的临时目录")
	}
	ctx := t.Context()
	test, resource, _, snapshots := newRecoveryAgentFixture(t)
	state, err := test.node.journal.read(resource.Specification.UserID)
	if err != nil || state == nil {
		t.Fatal("原容器生命周期记录缺失", err)
	}
	store, err := computeragent.NewFileStore(resource.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	desktop, err := store.Desktop(snapshots.desktop.Command.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(desktop.Save(ctx, snapshots.desktop), store.SaveBrowser(ctx, snapshots.browser), store.SaveProbe(ctx, snapshots.probe)); err != nil {
		t.Fatal(err)
	}
	startup := newAgentStartup(test.node)
	if err := startup.prepareRestart(ctx, resource, state); err == nil {
		t.Fatal("未取得代理锁即允许恢复")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := startup.prepareRestart(ctx, resource, state); err == nil {
		t.Fatal("原容器仍运行时允许离线恢复")
	}
	simulateComputerShutdown(t, test, resource, resource)
	if err := startup.prepareRestart(ctx, resource, state); err != nil {
		t.Fatal(err)
	}
	reopened, err := computeragent.NewFileStore(resource.ControlPath)
	if err != nil {
		t.Fatal("恢复结束后代理无法取得独占锁", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	desktop, err = reopened.Desktop(snapshots.desktop.Command.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	closedDesktop, desktopErr := desktop.Load(ctx)
	closedBrowser, browserErr := reopened.LoadBrowser(ctx)
	closedProbe, probeErr := reopened.LoadProbe(ctx)
	if err := errors.Join(desktopErr, browserErr, probeErr); err != nil {
		t.Fatal(err)
	}
	if closedDesktop == nil || closedBrowser == nil || closedProbe == nil || closedDesktop.Group == nil {
		t.Fatal("恢复丢失持久授权")
	}
	if closedDesktop.Phase != "closed" || closedDesktop.Command != snapshots.desktop.Command || *closedDesktop.Group != *snapshots.desktop.Group || closedBrowser.Phase != "closed" || closedBrowser.Grant != snapshots.browser.Grant || closedProbe.Phase != "closed" || closedProbe.Grant != snapshots.probe.Grant {
		t.Fatal("关闭屏障未刷盘或原授权被改动")
	}
}
