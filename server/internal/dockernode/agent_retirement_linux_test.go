package dockernode

import (
	"errors"
	"os"
	"testing"
)

func TestLinuxMetadataRetirementWithActualMountTable(t *testing.T) {
	f, resource := closeRetirementFixture(t)
	f.node.mounts = readMounts
	// 根挂载一定存在；只读实际 mountinfo，不创建挂载或要求额外内核权限。
	requireFailure(t, f.node.requireUnmountedControl("/"), "agent_retirement_directory_mounted")
	if _, err := f.node.Ensure(t.Context(), advanceSpecification(f.specification)); err != nil {
		t.Fatal("实际文件及挂载表下回收失败", err)
	}
	if _, err := os.Stat(resource.ControlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧目录未回收", err)
	}
}
