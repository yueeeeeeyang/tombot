package dockernode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/computer"
)

func TestDiskLayoutPreservesSharedAndBrowserContents(t *testing.T) {
	fixture := newStorageFixture(t)
	ctx := context.Background()
	if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{computer.WorkspaceSubdirectory, computer.BrowserSubdirectory} {
		path := filepath.Join(fixture.resource.WorkspacePath, name, "retained")
		if err := os.WriteFile(path, []byte("保留内容"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.command(ctx, "umount"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{computer.WorkspaceSubdirectory, computer.BrowserSubdirectory} {
		contents, err := os.ReadFile(filepath.Join(fixture.resource.WorkspacePath, name, "retained"))
		if err != nil || string(contents) != "保留内容" {
			t.Fatal("重新挂载丢失持久内容", err)
		}
	}
	if fixture.formats != 1 {
		t.Fatal("重新挂载触发格式化")
	}
}

func TestDiskLayoutRejectsDriftWithoutRepair(t *testing.T) {
	for _, change := range []string{"缺失", "链接", "权限", "旧根内容", "根权限"} {
		t.Run(change, func(t *testing.T) {
			fixture := newStorageFixture(t)
			ctx := context.Background()
			if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
				t.Fatal(err)
			}
			root := fixture.resource.WorkspacePath
			browser := filepath.Join(root, computer.BrowserSubdirectory)
			var err error
			switch change {
			case "缺失":
				err = os.Remove(browser)
			case "链接":
				if err = os.Remove(browser); err == nil {
					err = os.Symlink(t.TempDir(), browser)
				}
			case "权限":
				err = os.Chmod(browser, 0770)
			case "旧根内容":
				err = os.WriteFile(filepath.Join(root, "legacy"), []byte("不能转换"), 0600)
			case "根权限":
				err = os.Chmod(root, 0770)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fixture.storage.inspect(ctx, fixture.resource); err == nil {
				t.Fatal("漂移目录仍然报告就绪")
			}
			if _, err = fixture.storage.ensure(ctx, fixture.resource); err == nil {
				t.Fatal("已就绪目录被自动修复")
			}
			if change == "缺失" {
				if _, err = os.Lstat(browser); !os.IsNotExist(err) {
					t.Fatal("丢失目录被重新创建", err)
				}
			}
		})
	}
}

func TestDiskLayoutPartialCreationAndOldVersion(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		t.Run(map[bool]string{false: "空初始化恢复", true: "已有内容拒绝"}[nonempty], func(t *testing.T) {
			fixture := newStorageFixture(t)
			ctx := context.Background()
			fixture.failure = "mount_reply_lost"
			if _, err := fixture.storage.ensure(ctx, fixture.resource); err == nil {
				t.Fatal("挂载未知结果未报错")
			}
			if current, err := fixture.storage.inspect(ctx, fixture.resource); err != nil || current.Ready {
				t.Fatal("未完成布局提前就绪", err)
			}
			shared := filepath.Join(fixture.resource.WorkspacePath, computer.WorkspaceSubdirectory)
			if err := os.Mkdir(shared, 0700); err != nil {
				t.Fatal(err)
			}
			if nonempty {
				if err := os.WriteFile(filepath.Join(shared, "foreign"), []byte("不得修改"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fixture.failure = ""
			current, err := fixture.storage.ensure(ctx, fixture.resource)
			if nonempty {
				requireFailure(t, err, "storage_layout_initial_directory_not_empty")
			} else if err != nil || !current.Ready {
				t.Fatal("中断初始化未恢复", err)
			}
		})
	}
	fixture := newStorageFixture(t)
	state := initialStorageRecord(fixture.resource)
	state.Version = 2
	if err := fixture.storage.save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.ensure(context.Background(), fixture.resource); err == nil {
		t.Fatal("旧存储记录被自动转换")
	}
}

func TestContainerVolumeSubpathsAreValidated(t *testing.T) {
	for _, change := range []string{"顺序", "整卷", "交换子目录", "其他卷", "缺失", "重复", "复制镜像目录"} {
		t.Run(change, func(t *testing.T) {
			fixture := newFixture(t)
			resource := fixture.node.resource(fixture.specification)
			if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err != nil {
				t.Fatal(err)
			}
			value := fixture.engine.containers[resource.Name]
			mounts := value.HostConfig.Mounts
			switch change {
			case "顺序":
				mounts[0], mounts[1] = mounts[1], mounts[0]
			case "整卷":
				mounts[0].VolumeOptions.Subpath = ""
			case "交换子目录":
				mounts[0].VolumeOptions.Subpath, mounts[1].VolumeOptions.Subpath = mounts[1].VolumeOptions.Subpath, mounts[0].VolumeOptions.Subpath
			case "其他卷":
				mounts[1].Source = "foreign"
			case "缺失":
				value.Mounts = value.Mounts[:2]
			case "重复":
				mounts[1] = mounts[0]
			case "复制镜像目录":
				mounts[1].VolumeOptions.NoCopy = false
			}
			err := fixture.node.validateContainer(resource, value)
			if change == "顺序" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireFailure(t, err, "container_mount_mismatch")
			}
		})
	}
}
