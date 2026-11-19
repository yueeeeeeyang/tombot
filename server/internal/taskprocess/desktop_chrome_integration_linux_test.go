//go:build linux && storageintegration

package taskprocess

import (
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/computer"
)

// TestDesktopChromeDirectories 在临时 Linux 文件系统上检查正式引导使用的真实数值身份。
func TestDesktopChromeDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0711); err != nil {
		t.Fatal(err)
	}
	first, second := computer.DesktopBrowserUIDBase+1, computer.DesktopBrowserUIDBase+2
	if err := privateBrowserDirectory(root, "first", first, 0700, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "first", "kept")
	if err := os.WriteFile(file, []byte("保留配置"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "first", first, 0700, true); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "first", second, 0700, true); err == nil {
		t.Fatal("其他桌面取得已有配置")
	}
	if content, err := os.ReadFile(file); err != nil || string(content) != "保留配置" {
		t.Fatal("配置被替换", err)
	}
	if err := privateBrowserDirectory(root, "session", first, 0711, false); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "session", first, 0711, false); err == nil {
		t.Fatal("重复使用旧执行入口")
	}
	if err := os.Symlink("first", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "link", first, 0700, true); err == nil {
		t.Fatal("持久目录接受链接")
	}
	for _, name := range []string{"empty", "unexpected"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "unexpected", "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "empty", first, 0700, true); err != nil {
		t.Fatal(err)
	}
	if err := privateBrowserDirectory(root, "unexpected", first, 0700, true); err == nil {
		t.Fatal("含有文件的异常目录被更改归属")
	}
}
