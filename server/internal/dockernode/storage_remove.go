package dockernode

import (
	"context"
	"errors"
	"os"
	"time"
)

func (storage *FileStorage) remove(ctx context.Context, resource Resource) (Workspace, error) {
	state, err := storage.load(resource)
	if err != nil {
		return Workspace{}, err
	}
	if state == nil || state.Phase == "removed" {
		if err := storage.requireAbsentDirectory(resource); err != nil {
			return Workspace{}, err
		}
		if state == nil {
			state = initialStorageRecord(resource)
			state.Phase = "removed"
			return Workspace{}, storage.save(state)
		}
		return Workspace{}, nil
	}
	if err = storage.checkDirectory(resource, false); err != nil {
		return Workspace{}, err
	}
	mounted, err := storage.mounted(ctx, resource, state)
	if err != nil {
		return Workspace{}, err
	}
	state.Phase = "removing"
	if err = storage.save(state); err != nil {
		return Workspace{}, err
	}
	if mounted {
		if _, err = storage.command(ctx, "umount", "--no-canonicalize", resource.WorkspacePath); err != nil {
			return Workspace{}, err
		}
		if mounted, err = storage.mounted(ctx, resource, state); err != nil {
			return Workspace{}, err
		}
		if mounted {
			return Workspace{}, failure("storage_unmount_unconfirmed")
		}
	}
	devices, err := storage.loops(ctx, state)
	if err != nil {
		return Workspace{}, err
	}
	for _, device := range devices {
		if err = storage.verifyImage(resource, state, false); err != nil {
			return Workspace{}, err
		}
		if err = storage.detachDevice(ctx, state, device); err != nil {
			return Workspace{}, err
		}
	}
	if err = storage.confirmDetached(ctx, state); err != nil {
		return Workspace{}, err
	}
	image, err := storage.openImage(resource, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, errors.New("待清理镜像无法安全打开")
	}
	if err == nil {
		device, inode, size, _, identityErr := imageIdentity(image)
		closeErr := image.Close()
		if identityErr != nil || closeErr != nil {
			return Workspace{}, errors.Join(identityErr, closeErr)
		}
		if state.ImageInode == 0 && size != 0 || state.ImageInode != 0 && (device != state.ImageDevice || inode != state.ImageInode) {
			return Workspace{}, failure("storage_image_identity_mismatch")
		}
		if err = os.Remove(storage.image(resource)); err != nil {
			return Workspace{}, errors.New("删除已解除关联镜像失败")
		}
	}
	// 只删除已经卸载的空挂载点和本次资源目录，禁止递归删除未知内容或穿过嵌套挂载。
	for _, path := range []string{resource.WorkspacePath, storage.directory(resource)} {
		if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Workspace{}, errors.New("清理用户磁盘空目录失败")
		}
	}
	parent, err := os.Open(storage.options.Directory)
	if err != nil {
		return Workspace{}, errors.New("打开持久镜像父目录失败")
	}
	if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
		return Workspace{}, errors.New("同步持久镜像删除失败")
	}
	state.Phase = "removed"
	if err = storage.save(state); err != nil {
		return Workspace{}, err
	}
	return Workspace{}, nil
}

func (storage *FileStorage) detachDevice(ctx context.Context, state *storageRecord, device loopDevice) (result error) {
	release, err := storage.pinLoop(device, state)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	_, err = storage.command(ctx, "losetup", "--detach", device.Name)
	return err
}

func (storage *FileStorage) confirmDetached(ctx context.Context, state *storageRecord) error {
	for attempt := range 5 {
		devices, err := storage.loops(ctx, state)
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			return nil
		}
		if attempt == 4 {
			return failure("storage_loop_detach_unconfirmed")
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return failure("storage_loop_detach_unconfirmed")
}
