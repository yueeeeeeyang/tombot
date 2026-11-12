package computeragent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserFileRecordIntegrity(t *testing.T) {
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
	if snapshot, err := store.LoadBrowser(t.Context()); err != nil || snapshot != nil {
		t.Fatal("空共享浏览器记录错误", err)
	}
	snapshot := BrowserSnapshot{Version: 1, Grant: browserGrant(fixture), Phase: "consumed"}
	if err := store.SaveBrowser(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	actual, err := store.LoadBrowser(t.Context())
	if err != nil || actual == nil || *actual != snapshot {
		t.Fatal("共享浏览器消费记录没有完整往返", err)
	}
	snapshot.Phase = "closed"
	if err := store.SaveBrowser(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if actual, err = store.LoadBrowser(t.Context()); err != nil || actual == nil || *actual != snapshot {
		t.Fatal("共享浏览器关闭记录没有持久保存", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if store.SaveBrowser(ctx, snapshot) == nil {
		t.Fatal("取消后仍保存共享浏览器状态")
	}
	path := filepath.Join(directory, browserRecordName)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBrowser(t.Context()); err == nil {
		t.Fatal("共享可读记录被接受")
	}
	if store.SaveBrowser(t.Context(), snapshot) == nil {
		t.Fatal("保存修复了漂移权限")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(browserRecordName+".original", path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBrowser(t.Context()); err == nil {
		t.Fatal("共享浏览器记录接受符号链接")
	}
	if store.SaveBrowser(t.Context(), snapshot) == nil {
		t.Fatal("共享浏览器保存覆盖了符号链接")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadBrowser(t.Context()); err == nil {
		t.Fatal("损坏共享浏览器记录被视为空记录")
	}
}
