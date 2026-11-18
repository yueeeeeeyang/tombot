package dockernode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/computer"
)

func TestRestoreRetainedDiskKeepsWorkspaceAndBrowserData(t *testing.T) {
	ctx := t.Context()
	f := newStorageFixture(t)
	_, err := f.storage.restore(ctx, f.resource)
	requireFailure(t, err, "retained_storage_unconfirmed")
	if len(f.commands) != 0 {
		t.Fatal("恢复缺失的原盘时调用了宿主存储命令")
	}
	// 预置已格式化的原盘，避免将本测试依赖于测试宿主如何报告预分配的数据块。
	if err := f.storage.checkDirectory(f.resource, true); err != nil {
		t.Fatal(err)
	}
	image, err := f.storage.openImage(f.resource, true)
	if err != nil {
		t.Fatal(err)
	}
	state := initialStorageRecord(f.resource)
	device, inode, _, _, statErr := imageIdentity(image)
	if err := errors.Join(statErr, image.Truncate(state.CapacityBytes), image.Close()); err != nil {
		t.Fatal(err)
	}
	state.ImageDevice, state.ImageInode, state.Phase = device, inode, "formatted"
	if _, err := f.command(ctx, "mkfs"); err != nil {
		t.Fatal(err)
	}
	if err := f.storage.save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := f.storage.mountExisting(ctx, f.resource, state); err != nil {
		var nodeFailure *computer.Failure
		if errors.As(err, &nodeFailure) {
			t.Fatalf("预置测试磁盘无法挂载（%s）：%v", nodeFailure.Code, err)
		}
		t.Fatal(err)
	}
	for _, directory := range []string{computer.WorkspaceSubdirectory, computer.BrowserSubdirectory} {
		if err := os.WriteFile(filepath.Join(f.resource.WorkspacePath, directory, "retained"), []byte(directory), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.command(ctx, "umount"); err != nil {
		t.Fatal(err)
	}
	f.devices = []loopDevice{}
	if workspace, err := f.storage.restore(ctx, f.resource); err != nil || !workspace.Ready {
		t.Fatalf("无法恢复原挂载：%+v，%v", workspace, err)
	}
	current, err := f.storage.load(f.resource)
	if err != nil || current == nil || *state != *current || f.formats != 1 {
		t.Fatalf("恢复后磁盘身份变化或被重新格式化：%+v，%v", current, err)
	}
	for _, directory := range []string{computer.WorkspaceSubdirectory, computer.BrowserSubdirectory} {
		contents, err := os.ReadFile(filepath.Join(f.resource.WorkspacePath, directory, "retained"))
		if err != nil || string(contents) != directory {
			t.Fatal("重挂载未保留工作区或浏览器的数据", err)
		}
	}
}
