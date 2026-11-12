package dockernode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func TestDesktopJournalRejectsInvalidRecords(t *testing.T) {
	for _, scenario := range []string{"旧用户级名称", "名称缺少桌面", "名称含无效桌面", "内容属于另一桌面", "旧版本", "未知字段", "符号链接", "硬链接", "可写文件"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			session := fixture.prepareComputer(t, fixture.specification)
			key := sessionKeyOf(session)
			state := &sessionRecord{Version: 2, Command: commandFor(session), Epoch: ids.New(), Phase: "closed", Deadline: time.Now().UTC()}
			name := key.name() + ".json"
			switch scenario {
			case "旧用户级名称":
				name = session.UserID + ".session.json"
			case "名称缺少桌面":
				name = session.UserID + "..session.json"
			case "名称含无效桌面":
				name = session.UserID + ".invalid.session.json"
			case "内容属于另一桌面":
				state.Command.Session.DesktopID = ids.New()
			case "旧版本":
				state.Version = 1
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "未知字段" {
				encoded = append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
			}
			path := filepath.Join(fixture.node.options.StateDirectory, name)
			switch scenario {
			case "符号链接", "硬链接":
				target := filepath.Join(fixture.node.options.StateDirectory, "invalid-target.json")
				if err = os.WriteFile(target, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "符号链接" {
					err = os.Symlink(target, path)
				} else {
					err = os.Link(target, path)
				}
			default:
				err = os.WriteFile(path, encoded, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "可写文件" {
				if err = os.Chmod(path, 0660); err != nil {
					t.Fatal(err)
				}
			}
			groups := &fakeGroups{path: t.TempDir()}
			if controller, err := newSessions(ctx, fixture.node, groups, &fakeAuthority{}, sessionOptions(), sessionLogger()); err == nil {
				t.Error("无效记录未阻止启动")
				if err = controller.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			}
			before := len(fixture.engine.events)
			if _, err = fixture.node.Remove(ctx, fixture.specification); err == nil || len(fixture.engine.events) != before {
				t.Fatal("损坏桌面记录未阻止生命周期删除", err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(encoded) || groups.creates != 0 || groups.drains != 0 {
				t.Fatal("失败路径修改了旧记录或执行资源操作", err)
			}
		})
	}
}

func TestFailedCloseBeforeOpenRemainsTracked(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	session := fixture.prepareComputer(t, fixture.specification)
	groups := &fakeGroups{path: t.TempDir()}
	controller := fixture.sessionController(t, groups, sessionOptions())
	agent := controller.agent.(*fakeAuthority)
	agent.failClose = true
	t.Cleanup(func() { agent.failClose = false })
	command := commandFor(session)
	if _, err := controller.Close(ctx, command); err == nil {
		t.Fatal("未知关闭结果被确认")
	}
	if _, ok := controller.lease(sessionKeyOf(session)); !ok {
		t.Fatal("首次关闭失败不在回收和停服集合中")
	}
	requireDesktopState(t, controller, command, "quarantined")
	if err := controller.Shutdown(ctx); err == nil {
		t.Fatal("未确认的桌面关闭仍释放了服务锁")
	}
	agent.failClose = false
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	requireDesktopState(t, controller, command, "closed")
	if groups.creates != 0 || groups.drains != 0 {
		t.Fatal("未开启的桌面产生了资源组操作")
	}
}
