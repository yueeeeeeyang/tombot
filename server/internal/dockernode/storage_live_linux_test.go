//go:build storageintegration && linux

package dockernode

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func liveStorage(t *testing.T) (*FileStorage, Resource, FileStorageOptions) {
	t.Helper()
	root := os.Getenv("TOM_TEST_STORAGE_ROOT")
	if root != "/quota-test" {
		t.Fatal("存储验收只允许使用专属 /quota-test 卷")
	}
	directory, err := os.MkdirTemp(root, "storage-")
	if err != nil {
		t.Fatal(err)
	}
	options := FileStorageOptions{Directory: filepath.Join(directory, "images"), StateDirectory: filepath.Join(directory, "control"), CommandTimeout: 30 * time.Second, MinimumFreeBytes: 1 << 30}
	for _, path := range []string{options.Directory, options.StateDirectory} {
		if err = os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	storage, err := NewFileStorage(options)
	if err != nil {
		t.Fatal(err)
	}
	userID := ids.New()
	specification := computer.Specification{OperationID: ids.New(), UserID: userID, ComputerID: ids.New(), NodeID: ids.New(), ImageReference: "registry.example.test/computer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", VolumeReference: "tombot-user-" + userID,
		Generation: 1, FencingToken: 1, Attempt: 1, LeaseExpiresAt: time.Now().Add(5 * time.Minute), Resources: computer.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 512 << 20, ProcessLimit: 128}}
	node := &Runtime{options: Options{StorageDirectory: options.Directory}}
	resource := node.resource(specification)
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	return storage, resource, options
}

// TestLinuxStorageImageNoMount 验证真实预分配、ext4 格式及只读检查，不申请挂载或设备权限。
func TestLinuxStorageImageNoMount(t *testing.T) {
	storage, resource, _ := liveStorage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state := initialStorageRecord(resource)
	state.Phase = "allocating"
	if err := storage.save(state); err != nil {
		t.Fatal(err)
	}
	if err := storage.checkDirectory(resource, true); err != nil {
		t.Fatal(err)
	}
	if err := storage.allocate(ctx, resource, state); err != nil {
		t.Fatal(err)
	}
	state.Phase = "formatting"
	if err := storage.save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.command(ctx, "mkfs", "-q", "-F", "-m", "0", "-U", state.UUID, "-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0", storage.image(resource)); err != nil {
		t.Fatal(err)
	}
	if err := storage.finishFormat(resource, state); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.command(ctx, "fsck", "-f", "-n", storage.image(resource)); err != nil {
		t.Fatal(err)
	}
	image, err := storage.openImage(resource, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, size, allocated, err := imageIdentity(image)
	err = errors.Join(err, image.Close())
	if err != nil || size != 512<<20 || allocated < size {
		t.Fatal("实际镜像没有保留硬容量预分配", err)
	}
	workspace, err := storage.Inspect(ctx, resource)
	if err != nil || !workspace.Exists || workspace.Ready {
		t.Fatal("未挂载镜像被误报就绪", err)
	}
	t.Log("实际 512 MiB 镜像预分配、固定 UUID、ext4 格式及只读完整性检查通过；未执行挂载验收")
}

func prepareTestLoopDevices(t *testing.T) {
	t.Helper()
	for index := -1; index < 64; index++ {
		path := "/dev/loop-control"
		mode := uint32(unix.S_IFCHR | 0600)
		device := unix.Mkdev(10, 237)
		if index >= 0 {
			path = "/dev/loop" + strconv.Itoa(index)
			mode = unix.S_IFBLK | 0600
			device = unix.Mkdev(7, uint32(index))
		}
		var stat unix.Stat_t
		err := unix.Lstat(path, &stat)
		if errors.Is(err, os.ErrNotExist) {
			if err = unix.Mknod(path, mode, int(device)); err != nil {
				t.Fatal("测试节点无法创建所需设备入口", err)
			}
			continue
		}
		if err != nil || uint64(stat.Rdev) != device || stat.Mode&unix.S_IFMT != mode&unix.S_IFMT {
			t.Fatal("测试设备入口与预期不符", err)
		}
	}
}

// TestLinuxStorageQuotaAndRecovery 必须显式授权节点级设备权限；在专属测试卷验证真实硬容量和恢复。
func TestLinuxStorageQuotaAndRecovery(t *testing.T) {
	if os.Getenv("TOM_TEST_STORAGE_KERNEL") != "1" {
		t.Fatal("内核存储验收必须显式启用，不能跳过所需权限")
	}
	prepareTestLoopDevices(t)
	storage, resource, options := liveStorage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := storage.Remove(cleanup, resource); err != nil {
			t.Error("测试磁盘未完全回收，必须保留测试卷与记录", err)
		}
	})
	workspace, err := storage.Ensure(ctx, resource)
	if err != nil || !workspace.Ready {
		t.Fatal("真实用户磁盘未就绪", err)
	}
	persisted := []byte("持久磁盘重启后仍需保留此文件")
	if err = os.WriteFile(filepath.Join(resource.WorkspacePath, computer.WorkspaceSubdirectory, "persisted.txt"), persisted, 0600); err != nil {
		t.Fatal(err)
	}
	fill, err := os.OpenFile(filepath.Join(resource.WorkspacePath, computer.WorkspaceSubdirectory, "fill.bin"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1<<20)
	written := int64(0)
	var writeErr error
	for written <= resource.Specification.Resources.DiskBytes {
		var count int
		count, writeErr = fill.Write(buffer)
		written += int64(count)
		if writeErr != nil {
			break
		}
	}
	closeErr := fill.Close()
	if !errors.Is(writeErr, unix.ENOSPC) || written <= 0 || written > resource.Specification.Resources.DiskBytes || closeErr != nil {
		t.Fatal("真实文件系统未按容量拒绝继续写入", writeErr, closeErr)
	}
	if err = os.Remove(filepath.Join(resource.WorkspacePath, computer.WorkspaceSubdirectory, "fill.bin")); err != nil {
		t.Fatal(err)
	}
	state, err := storage.load(resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.command(ctx, "umount", "--no-canonicalize", resource.WorkspacePath); err != nil {
		t.Fatal(err)
	}
	devices, err := storage.loops(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if err = storage.detachDevice(ctx, state, device); err != nil {
			t.Fatal(err)
		}
	}
	if err = storage.confirmDetached(ctx, state); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewFileStorage(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	workspace, err = restarted.Inspect(ctx, resource)
	if err != nil || !workspace.Exists || workspace.Ready {
		t.Fatal("重启前丢失挂载的状态不符", err)
	}
	workspace, err = restarted.Ensure(ctx, resource)
	if err != nil || !workspace.Ready {
		t.Fatal("重新挂载失败", err)
	}
	contents, err := os.ReadFile(filepath.Join(resource.WorkspacePath, computer.WorkspaceSubdirectory, "persisted.txt"))
	if err != nil || !bytes.Equal(contents, persisted) {
		t.Fatal("恢复挂载后用户文件丢失", err)
	}
	busy, err := os.Open(filepath.Join(resource.WorkspacePath, computer.WorkspaceSubdirectory, "persisted.txt"))
	if err != nil {
		t.Fatal(err)
	}
	busyClosed := false
	t.Cleanup(func() {
		if !busyClosed {
			if err := busy.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err = restarted.Remove(ctx, resource); err == nil {
		t.Fatal("仍有打开文件时删除了磁盘")
	}
	if err = busy.Close(); err != nil {
		t.Fatal(err)
	}
	busyClosed = true
	if err = restarted.Remove(ctx, resource); err != nil {
		t.Fatal("释放文件后清理失败", err)
	}
	workspace, err = restarted.Inspect(ctx, resource)
	if err != nil || workspace.Exists {
		t.Fatal("资源清理未确认", err)
	}
	devices, err = restarted.loops(ctx, state)
	if err != nil || len(devices) != 0 {
		t.Fatal("测试仍遗留磁盘关联", err)
	}
	t.Log("实际硬容量耗尽、数据持久化、重新挂载、占用拒删及完整回收均通过")
}
