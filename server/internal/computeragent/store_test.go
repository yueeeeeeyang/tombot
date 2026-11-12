package computeragent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/infra/ids"
)

func TestFileStoreIntegrityAndExclusiveOwnership(t *testing.T) {
	fixture := newFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openFileStore(directory, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if second, err := openFileStore(directory, uint32(os.Geteuid())); err == nil {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("两个代理持有同一控制目录")
	}
	record, err := store.Desktop(fixture.command.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := record.Load(context.Background()); err != nil || value != nil {
		t.Fatal("空记录读取失败", err)
	}
	snapshot := Snapshot{Version: 2, Command: fixture.command, Group: &fixture.reference, Phase: "closed"}
	if err := record.Save(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := record.Load(context.Background())
	if err != nil || !reflect.DeepEqual(loaded, &snapshot) {
		t.Fatal("控制记录未持久往返", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := record.Save(ctx, snapshot); err == nil {
		t.Fatal("取消后仍保存记录")
	}
	if err := os.Link(filepath.Join(directory, fixture.command.Session.DesktopID+".session.json"), filepath.Join(directory, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Load(context.Background()); err == nil {
		t.Fatal("硬链接记录被接受")
	}
	if err := record.Save(context.Background(), snapshot); err == nil {
		t.Fatal("保存覆盖了不安全记录")
	}
	if err := os.Remove(filepath.Join(directory, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, fixture.command.Session.DesktopID+".session.json"), filepath.Join(directory, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original", filepath.Join(directory, fixture.command.Session.DesktopID+".session.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Load(context.Background()); err == nil {
		t.Fatal("符号链接记录被接受")
	}
	if err := record.Save(context.Background(), snapshot); err == nil {
		t.Fatal("保存修复了意外替换的记录")
	}
	if err := os.Remove(filepath.Join(directory, fixture.command.Session.DesktopID+".session.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, "original"), filepath.Join(directory, fixture.command.Session.DesktopID+".session.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, fixture.command.Session.DesktopID+".session.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Load(context.Background()); err == nil {
		t.Fatal("损坏记录被默认成空状态")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Load(context.Background()); err == nil {
		t.Fatal("关闭后仍访问记录")
	}
}

func TestDesktopFileRecordsAndRecovery(t *testing.T) {
	fixture := newFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := openFileStore(directory, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	first := Snapshot{Version: 2, Command: fixture.command, Phase: "closed"}
	secondCommand, _ := desktopCommand(fixture.command, 2)
	second := Snapshot{Version: 2, Command: secondCommand, Phase: "closed"}
	var pending sync.WaitGroup
	errors := make(chan error, 2)
	for _, snapshot := range []Snapshot{first, second} {
		pending.Add(1)
		go func() {
			defer pending.Done()
			record, err := store.Desktop(snapshot.Command.Session.DesktopID)
			if err == nil {
				err = record.Save(context.Background(), snapshot)
			}
			errors <- err
		}()
	}
	pending.Wait()
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	controller, err := NewController(context.Background(), fixture.guard.identity, store, fixture.factory, fixture.guard.options, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(controller.desktops) != 2 {
		t.Fatal("恢复遗漏持久桌面")
	}
	if err = controller.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Desktop(first.Command.Session.DesktopID)
	if err != nil {
		t.Fatal(err)
	}
	if err = record.Save(context.Background(), second); err == nil {
		t.Fatal("记录入口覆盖了其他桌面")
	}
	for _, invalid := range []string{"", "../escape", ids.New() + "/session"} {
		if _, err = store.Desktop(invalid); err == nil {
			t.Fatal("非法路径成为控制记录入口")
		}
	}
}

func TestFileCatalogRejectsLegacyAndWrongDesktop(t *testing.T) {
	for _, scenario := range []string{"旧单会话文件", "旧记录版本", "错误桌面归属", "无效文件名"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t)
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := openFileStore(directory, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			state := Snapshot{Version: 2, Command: fixture.command, Phase: "closed"}
			name := state.Command.Session.DesktopID + ".session.json"
			switch scenario {
			case "旧单会话文件":
				name = "session.json"
			case "旧记录版本":
				state.Version = 1
			case "错误桌面归属":
				state.Command.Session.DesktopID = ids.New()
			case "无效文件名":
				name = "invalid.session.json"
			}
			encoded, err := agentchannel.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, name)
			if err = os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = NewController(context.Background(), fixture.guard.identity, store, fixture.factory, fixture.guard.options, 2, fixture.guard.logger); err == nil {
				t.Fatal("恢复接受了旧记录或错误归属")
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(encoded) {
				t.Fatal("失败恢复改写了旧记录", err)
			}
		})
	}
}
