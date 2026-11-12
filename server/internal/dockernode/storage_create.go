package dockernode

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
)

func (storage *FileStorage) ensure(ctx context.Context, resource Resource) (Workspace, error) {
	current, err := storage.inspect(ctx, resource)
	if err != nil {
		return Workspace{}, err
	}
	state, err := storage.load(resource)
	if err != nil {
		return Workspace{}, err
	}
	if state == nil {
		state = initialStorageRecord(resource)
		if err = storage.save(state); err != nil {
			return Workspace{}, err
		}
	}
	if state.Phase == "removing" || state.Phase == "removed" {
		return Workspace{}, failure("storage_generation_closed")
	}
	if current.Ready {
		return current, nil
	}
	if err = storage.checkDirectory(resource, true); err != nil {
		return Workspace{}, err
	}
	if state.Phase == "reserved" {
		state.Phase = "allocating"
		if err = storage.save(state); err != nil {
			return Workspace{}, err
		}
	}
	if state.Phase == "allocating" {
		if err = storage.allocate(ctx, resource, state); err != nil {
			return Workspace{}, err
		}
		state.Phase = "formatting"
		if err = storage.save(state); err != nil {
			return Workspace{}, err
		}
		if _, err = storage.command(ctx, "mkfs", "-q", "-F", "-m", "0", "-U", state.UUID, "-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0", storage.image(resource)); err != nil {
			return Workspace{}, err
		}
		if err = storage.finishFormat(resource, state); err != nil {
			return Workspace{}, err
		}
	} else if state.Phase == "formatting" {
		// 格式化回包丢失不能再次覆盖镜像，只读检查能证明完整时才继续，否则交由开户补偿处理。
		if err = storage.verifyImage(resource, state, true); err != nil {
			return Workspace{}, err
		}
		if _, err = storage.command(ctx, "fsck", "-f", "-n", storage.image(resource)); err != nil {
			return Workspace{}, failure("storage_format_unconfirmed")
		}
		if err = storage.finishFormat(resource, state); err != nil {
			return Workspace{}, err
		}
	}
	if err = storage.verifyImage(resource, state, true); err != nil {
		return Workspace{}, err
	}
	// mount 成功但回包或 ready 记录丢失时，恢复布局而不是把已挂载的非空文件系统再次挂载。
	mounted, err := storage.mounted(ctx, resource, state)
	if err != nil {
		return Workspace{}, err
	}
	if mounted {
		return storage.finishLayout(ctx, resource, state)
	}
	device, err := storage.attach(ctx, resource, state)
	if err != nil {
		return Workspace{}, err
	}
	if err = os.Mkdir(resource.WorkspacePath, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return Workspace{}, errors.New("创建用户磁盘挂载点失败")
	}
	if err = checkProtectedDirectory(resource.WorkspacePath, storage.owner); err != nil {
		return Workspace{}, err
	}
	entries, err := os.ReadDir(resource.WorkspacePath)
	if err != nil {
		return Workspace{}, errors.New("读取用户磁盘挂载点失败")
	}
	if len(entries) != 0 {
		return Workspace{}, failure("storage_mountpoint_not_empty")
	}
	if err = storage.mountDevice(ctx, resource, state, device); err != nil {
		return Workspace{}, err
	}
	mounted, err = storage.mounted(ctx, resource, state)
	if err != nil {
		return Workspace{}, err
	}
	if !mounted {
		return Workspace{}, failure("storage_mount_unconfirmed")
	}
	return storage.finishLayout(ctx, resource, state)
}

func (storage *FileStorage) finishLayout(ctx context.Context, resource Resource, state *storageRecord) (Workspace, error) {
	if err := ctx.Err(); err != nil {
		return Workspace{}, err
	}
	if err := storage.layout.prepare(resource.WorkspacePath, state.Phase == "formatted"); err != nil {
		return Workspace{}, err
	}
	if err := ctx.Err(); err != nil {
		return Workspace{}, err
	}
	state.Phase = "ready"
	if err := storage.save(state); err != nil {
		return Workspace{}, err
	}
	return storage.workspace(resource, true), nil
}

func (storage *FileStorage) mountDevice(ctx context.Context, resource Resource, state *storageRecord, device loopDevice) (result error) {
	release, err := storage.pinLoop(device, state)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	_, err = storage.command(ctx, "mount", "--no-canonicalize", "--types", "ext4", "--options", "rw,nosuid,nodev", device.Name, resource.WorkspacePath)
	return err
}

func (storage *FileStorage) allocate(ctx context.Context, resource Resource, state *storageRecord) (result error) {
	var image *os.File
	var err error
	if state.ImageInode == 0 {
		image, err = storage.openImage(resource, true)
		if errors.Is(err, os.ErrExist) {
			image, err = storage.openImage(resource, false)
		}
		if err != nil {
			return errors.New("创建持久镜像失败")
		}
		device, inode, size, _, identityErr := imageIdentity(image)
		if identityErr != nil || size != 0 {
			return errors.Join(failure("storage_new_image_not_empty"), image.Close())
		}
		state.ImageDevice = device
		state.ImageInode = inode
		if err = storage.save(state); err != nil {
			return errors.Join(err, image.Close())
		}
	} else {
		image, err = storage.openImage(resource, false)
		if err != nil {
			return errors.New("持久镜像分配记录与文件不符")
		}
	}
	defer func() { result = errors.Join(result, image.Close()) }()
	device, inode, size, allocated, err := imageIdentity(image)
	if err != nil {
		return err
	}
	if device != state.ImageDevice || inode != state.ImageInode || size > state.CapacityBytes || allocated < 0 {
		return failure("storage_image_identity_mismatch")
	}
	available, err := storage.freeBytes(storage.options.Directory)
	if err != nil {
		return err
	}
	needed := uint64(max(int64(0), state.CapacityBytes-allocated))
	if available < storage.options.MinimumFreeBytes || needed > available-storage.options.MinimumFreeBytes {
		return failure("storage_capacity_unavailable")
	}
	if _, err = storage.command(ctx, "fallocate", "--length", strconv.FormatInt(state.CapacityBytes, 10), "--", storage.image(resource)); err != nil {
		return err
	}
	_, _, size, allocated, err = imageIdentity(image)
	if err != nil {
		return err
	}
	if size != state.CapacityBytes || allocated < state.CapacityBytes {
		return failure("storage_allocation_unconfirmed")
	}
	if err = image.Sync(); err != nil {
		return errors.New("同步持久镜像分配失败")
	}
	return nil
}

func (storage *FileStorage) finishFormat(resource Resource, state *storageRecord) (result error) {
	if err := storage.verifyImage(resource, state, true); err != nil {
		return err
	}
	image, err := storage.openImage(resource, false)
	if err != nil {
		return errors.New("打开已格式化镜像失败")
	}
	if err = errors.Join(image.Sync(), image.Close()); err != nil {
		return errors.New("同步文件系统初始化失败")
	}
	state.Phase = "formatted"
	return storage.save(state)
}

func (storage *FileStorage) attach(ctx context.Context, resource Resource, state *storageRecord) (device loopDevice, result error) {
	// losetup --find 不是跨进程原子操作，节点共享独占分配锁；内核或外部竞争失败仍明确向上返回。
	release, err := storage.journal.lock(ctx, "loop-allocation")
	if err != nil {
		return device, err
	}
	defer func() { result = errors.Join(result, release()) }()
	devices, err := storage.loops(ctx, state)
	if err != nil {
		return device, err
	}
	if len(devices) == 1 {
		return devices[0], nil
	}
	encoded, err := storage.command(ctx, "losetup", "--find", "--show", "--nooverlap", "--sizelimit", strconv.FormatInt(state.CapacityBytes, 10), storage.image(resource))
	if err != nil {
		return device, err
	}
	name := strings.TrimSpace(string(encoded))
	if !validLoopDevice(name) {
		return device, failure("storage_loop_result_invalid")
	}
	devices, err = storage.loops(ctx, state)
	if err != nil {
		return device, err
	}
	if len(devices) != 1 || devices[0].Name != name {
		return device, failure("storage_loop_setup_unconfirmed")
	}
	return devices[0], nil
}
