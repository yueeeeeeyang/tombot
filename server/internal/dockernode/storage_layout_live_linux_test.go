//go:build storageintegration && linux

package dockernode

import (
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/computer"
)

// TestLinuxStorageLayout 只在新临时目录验证实际数值身份，不申请挂载、loop 或宿主权限。
func TestLinuxStorageLayout(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("目录身份验收要求临时容器 root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	layout := diskLayoutIdentity{rootUID: 0, rootGID: 0, sharedGID: computer.DesktopSharedGID, browserUID: computer.BrowserUID}
	if err := layout.prepare(root, true); err != nil {
		t.Fatal(err)
	}
	if err := layout.prepare(root, false); err != nil {
		t.Fatal(err)
	}
	browser := filepath.Join(root, computer.BrowserSubdirectory)
	if err := os.Chown(browser, computer.DesktopUIDBase+1, computer.DesktopSharedGID); err != nil {
		t.Fatal(err)
	}
	if err := layout.prepare(root, false); err == nil {
		t.Fatal("真实 Bot 身份替换浏览器服务身份未被拒绝")
	}
	if err := layout.prepare(root, true); err == nil {
		t.Fatal("异常所属身份被自动修复")
	}
}
