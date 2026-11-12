package dockernode

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 存储编排测试使用小型文件和可控内核替身；实际容量与 ext4 行为由 Linux 实机测试验证。
type storageFixture struct {
	storage   *FileStorage
	resource  Resource
	devices   []loopDevice
	mounted   []filesystemMount
	commands  []string
	failure   string
	available uint64
	formats   int
	backing   string
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	node := newFixture(t)
	specification := node.specification
	specification.Resources.DiskBytes = 2 << 20
	resource := node.node.resource(specification)
	journal, err := openJournal(t.TempDir(), uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &storageFixture{resource: resource, devices: []loopDevice{}, available: 10 << 30, backing: filepath.Join(t.TempDir(), "unmounted")}
	storage := &FileStorage{options: FileStorageOptions{Directory: node.node.options.StorageDirectory, StateDirectory: journal.root.Name(), CommandTimeout: time.Second, MinimumFreeBytes: 1 << 30}, journal: journal, owner: uint32(os.Geteuid())}
	storage.layout = diskLayoutIdentity{rootUID: os.Geteuid(), rootGID: os.Getegid(), sharedGID: os.Getegid(), browserUID: os.Geteuid()}
	storage.freeBytes = func(string) (uint64, error) { return fixture.available, nil }
	storage.filesystemSize = func(string) (int64, error) { return specification.Resources.DiskBytes - 4096, nil }
	storage.mounts = func() ([]filesystemMount, error) { return fixture.mounted, nil }
	storage.pinLoop = func(loopDevice, *storageRecord) (func() error, error) { return func() error { return nil }, nil }
	storage.run = fixture.command
	fixture.storage = storage
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func (fixture *storageFixture) command(_ context.Context, program string, arguments ...string) (storageResult, error) {
	fixture.commands = append(fixture.commands, program)
	if fixture.failure == program {
		return storageResult{}, context.DeadlineExceeded
	}
	path := fixture.storage.image(fixture.resource)
	switch program {
	case "fallocate":
		size, err := strconv.Atoi(arguments[1])
		if err != nil {
			return storageResult{}, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return storageResult{}, err
		}
		_, err = file.Write(make([]byte, size))
		return storageResult{}, errors.Join(err, file.Sync(), file.Close())
	case "mkfs":
		fixture.formats++
		if fixture.failure == "format_incomplete" {
			return storageResult{}, context.DeadlineExceeded
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return storageResult{}, err
		}
		var superblock [120]byte
		binary.LittleEndian.PutUint16(superblock[0x38:0x3a], 0xef53)
		uuid, err := hex.DecodeString(strings.ReplaceAll(filesystemUUID(fixture.resource), "-", ""))
		if err != nil {
			return storageResult{}, errors.Join(err, file.Close())
		}
		copy(superblock[0x68:0x78], uuid)
		_, err = file.WriteAt(superblock[:], 1024)
		err = errors.Join(err, file.Close())
		if err == nil && fixture.failure == "format_reply_lost" {
			return storageResult{}, context.DeadlineExceeded
		}
		return storageResult{}, err
	case "fsck":
		return storageResult{}, nil
	case "losetup":
		switch arguments[0] {
		case "--list":
			encoded, err := json.Marshal(map[string]any{"loopdevices": fixture.devices})
			return storageResult{output: encoded}, err
		case "--find":
			file, err := os.Open(path)
			if err != nil {
				return storageResult{}, err
			}
			device, inode, _, _, err := imageIdentity(file)
			err = errors.Join(err, file.Close())
			if err != nil {
				return storageResult{}, err
			}
			fixture.devices = []loopDevice{{Name: "/dev/loop7", BackingFile: path, BackingInode: inode, BackingDevice: deviceIdentity(device), Device: "7:7", SizeLimit: fixture.resource.Specification.Resources.DiskBytes}}
			return storageResult{output: []byte("/dev/loop7\n")}, nil
		case "--detach":
			if fixture.failure != "detach_deferred" {
				fixture.devices = []loopDevice{}
			}
			return storageResult{}, nil
		}
	case "mount":
		if _, err := os.Stat(fixture.backing); err == nil {
			if err = os.Remove(fixture.resource.WorkspacePath); err != nil {
				return storageResult{}, err
			}
			if err = os.Rename(fixture.backing, fixture.resource.WorkspacePath); err != nil {
				return storageResult{}, err
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err = os.Chown(fixture.resource.WorkspacePath, os.Geteuid(), os.Getegid()); err != nil {
				return storageResult{}, err
			}
			if err = os.Chmod(fixture.resource.WorkspacePath, 0755); err != nil {
				return storageResult{}, err
			}
		} else {
			return storageResult{}, err
		}
		fixture.mounted = []filesystemMount{{target: fixture.resource.WorkspacePath, source: "/dev/loop7", root: "/", kind: "ext4", device: "7:7", options: map[string]bool{"rw": true, "nosuid": true, "nodev": true}}}
		if fixture.failure == "mount_reply_lost" {
			return storageResult{}, context.DeadlineExceeded
		}
		return storageResult{}, nil
	case "umount":
		// 假挂载也必须模拟卸载后内容不可见，否则会错误地要求生产代码递归删除数据目录。
		if err := os.Rename(fixture.resource.WorkspacePath, fixture.backing); err != nil {
			return storageResult{}, err
		}
		if err := os.Mkdir(fixture.resource.WorkspacePath, 0755); err != nil {
			return storageResult{}, err
		}
		fixture.mounted = nil
		return storageResult{}, nil
	}
	return storageResult{}, errors.New("测试出现未声明的宿主动作")
}

func TestPersistentStorageStateMachine(t *testing.T) {
	ctx := context.Background()
	fixture := newStorageFixture(t)
	workspace, err := fixture.storage.ensure(ctx, fixture.resource)
	if err != nil || !workspace.Exists || !workspace.Ready {
		t.Fatal("磁盘供给未就绪", err)
	}
	if _, err = fixture.storage.ensure(ctx, fixture.resource); err != nil {
		t.Fatal(err)
	}
	if fixture.formats != 1 {
		t.Fatal("重复请求格式化了已有镜像")
	}
	if _, err = fixture.command(ctx, "umount"); err != nil {
		t.Fatal(err)
	}
	fixture.devices = []loopDevice{}
	workspace, err = fixture.storage.inspect(ctx, fixture.resource)
	if err != nil || !workspace.Exists || workspace.Ready {
		t.Fatal("丢失挂载仍被视为可用", err)
	}
	if _, err = fixture.storage.ensure(ctx, fixture.resource); err != nil {
		t.Fatal(err)
	}
	if fixture.formats != 1 {
		t.Fatal("重启恢复重新格式化了持久数据")
	}
	if _, err = fixture.storage.remove(ctx, fixture.resource); err != nil {
		t.Fatal(err)
	}
	workspace, err = fixture.storage.inspect(ctx, fixture.resource)
	if err != nil || workspace.Exists {
		t.Fatal("清理结果未确认", err)
	}
	if _, err = fixture.storage.remove(ctx, fixture.resource); err != nil {
		t.Fatal("重复清理不幂等", err)
	}
	_, err = fixture.storage.ensure(ctx, fixture.resource)
	requireFailure(t, err, "storage_generation_closed")
}

func TestStorageUnknownResultsAndCleanupBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"format_reply_lost", "mount_reply_lost"} {
		t.Run(reason, func(t *testing.T) {
			fixture := newStorageFixture(t)
			fixture.failure = reason
			if _, err := fixture.storage.ensure(ctx, fixture.resource); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			fixture.failure = ""
			workspace, err := fixture.storage.ensure(ctx, fixture.resource)
			if err != nil || !workspace.Ready {
				t.Fatal("未知结果未恢复", err)
			}
			if fixture.formats != 1 {
				t.Fatal("未知结果导致重复格式化")
			}
		})
	}
	t.Run("格式化未完成不能再次覆盖", func(t *testing.T) {
		fixture := newStorageFixture(t)
		fixture.failure = "format_incomplete"
		if _, err := fixture.storage.ensure(ctx, fixture.resource); err == nil {
			t.Fatal("格式化失败被忽略")
		}
		fixture.failure = ""
		if _, err := fixture.storage.ensure(ctx, fixture.resource); err == nil {
			t.Fatal("未完成格式化被视为完整文件系统")
		}
		if fixture.formats != 1 {
			t.Fatal("对账重复格式化了镜像")
		}
		if _, err := fixture.storage.remove(ctx, fixture.resource); err != nil {
			t.Fatal("失败供给未能清理", err)
		}
	})
	for _, reason := range []string{"umount", "detach_deferred"} {
		t.Run(reason, func(t *testing.T) {
			fixture := newStorageFixture(t)
			if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
				t.Fatal(err)
			}
			fixture.failure = reason
			if _, err := fixture.storage.remove(ctx, fixture.resource); err == nil {
				t.Fatal("未确认释放设备就报告清理成功")
			}
			if _, err := os.Stat(fixture.storage.image(fixture.resource)); err != nil {
				t.Fatal("仍被占用的镜像已被删除")
			}
			fixture.failure = ""
			if _, err := fixture.storage.remove(ctx, fixture.resource); err != nil {
				t.Fatal("清理恢复失败", err)
			}
		})
	}
	t.Run("容量不足不启动格式化", func(t *testing.T) {
		fixture := newStorageFixture(t)
		fixture.available = 1 << 30
		_, err := fixture.storage.ensure(ctx, fixture.resource)
		requireFailure(t, err, "storage_capacity_unavailable")
		if fixture.formats != 0 {
			t.Fatal("容量不足仍格式化")
		}
		if _, err = fixture.storage.remove(ctx, fixture.resource); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStorageRejectsForeignResources(t *testing.T) {
	ctx := context.Background()
	t.Run("业务卷禁止执行不能标为就绪", func(t *testing.T) {
		fixture := newStorageFixture(t)
		if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
			t.Fatal(err)
		}
		fixture.mounted[0].options["noexec"] = true
		before := len(fixture.commands)
		_, err := fixture.storage.ensure(ctx, fixture.resource)
		requireFailure(t, err, "storage_mount_invalid")
		for _, command := range fixture.commands[before:] {
			if command == "mount" || command == "umount" || command == "mkfs.ext4" {
				t.Fatal("对账修改了现有业务卷")
			}
		}
	})
	t.Run("未知目录不收养", func(t *testing.T) {
		fixture := newStorageFixture(t)
		if err := os.Mkdir(fixture.storage.directory(fixture.resource), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.storage.ensure(ctx, fixture.resource)
		requireFailure(t, err, "storage_unowned_directory")
	})
	t.Run("错误挂载不得清理", func(t *testing.T) {
		fixture := newStorageFixture(t)
		if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
			t.Fatal(err)
		}
		fixture.mounted[0].source = "/dev/loop8"
		before := len(fixture.commands)
		_, err := fixture.storage.remove(ctx, fixture.resource)
		requireFailure(t, err, "storage_mount_identity_mismatch")
		for _, command := range fixture.commands[before:] {
			if command == "umount" {
				t.Fatal("卸载了错误的文件系统")
			}
		}
	})
	t.Run("镜像 inode 更换不得复用", func(t *testing.T) {
		fixture := newStorageFixture(t)
		if _, err := fixture.storage.ensure(ctx, fixture.resource); err != nil {
			t.Fatal(err)
		}
		path := fixture.storage.image(fixture.resource)
		if err := os.Rename(path, filepath.Join(t.TempDir(), "original.img")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 2<<20), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.storage.remove(ctx, fixture.resource)
		requireFailure(t, err, "storage_image_identity_mismatch")
	})
	t.Run("调用者不能覆盖宿主路径", func(t *testing.T) {
		fixture := newStorageFixture(t)
		fixture.resource.WorkspacePath = "/etc"
		_, err := fixture.storage.Ensure(ctx, fixture.resource)
		requireFailure(t, err, "storage_identity_invalid")
		if len(fixture.commands) != 0 {
			t.Fatal("错误路径到达宿主工具")
		}
	})
}

func TestMountRecordParsing(t *testing.T) {
	encoded := []byte("36 35 7:0 / /disk/user\\040space rw,nosuid,nodev shared:4 - ext4 /dev/loop0 rw\n")
	mounts, err := parseMounts(encoded)
	if err != nil || len(mounts) != 1 || mounts[0].target != "/disk/user space" || !mounts[0].options["nosuid"] {
		t.Fatal("挂载记录解析错误", err)
	}
	for _, value := range []string{"invalid", "1 2 7:0 / /wrong\\777 rw - ext4 /dev/loop0 rw", "1 2 7:0 / /path rw - ext4"} {
		if _, err = parseMounts([]byte(value)); err == nil {
			t.Fatal("损坏挂载记录未被拒绝")
		}
	}
	for _, name := range []string{"/dev/loop0", "/dev/loop123"} {
		if !validLoopDevice(name) {
			t.Fatal("合法设备名未通过")
		}
	}
	for _, name := range []string{"/dev/loop01", "/dev/loop0/other", "/dev/sda", "/dev/loop-1"} {
		if validLoopDevice(name) {
			t.Fatal("不受控设备名被接受")
		}
	}
}
