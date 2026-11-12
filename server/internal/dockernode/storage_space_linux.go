package dockernode

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// pinLoopDevice 用打开的设备引用固定当前关联，避免检查后设备编号被解除并复用于另一个镜像。
func pinLoopDevice(device loopDevice, state *storageRecord) (func() error, error) {
	file, err := os.OpenFile(device.Name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("打开已核验磁盘设备失败")
	}
	status, err := unix.IoctlLoopGetStatus64(int(file.Fd()))
	if err != nil {
		return nil, errors.Join(errors.New("核验内核磁盘关联失败"), file.Close())
	}
	if status.Device != state.ImageDevice || status.Inode != state.ImageInode || status.Offset != 0 || status.Sizelimit != uint64(state.CapacityBytes) || status.Flags&unix.LO_FLAGS_READ_ONLY != 0 {
		return nil, errors.Join(failure("storage_kernel_identity_mismatch"), file.Close())
	}
	return file.Close, nil
}

func availableStorageBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, errors.New("读取宿主磁盘余量失败")
	}
	if stat.Bsize <= 0 || stat.Bavail > math.MaxUint64/uint64(stat.Bsize) {
		return 0, errors.New("宿主磁盘容量无效")
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func mountedFilesystemSize(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, errors.New("读取用户文件系统容量失败")
	}
	if stat.Type != unix.EXT4_SUPER_MAGIC || stat.Bsize <= 0 || stat.Blocks > math.MaxInt64/uint64(stat.Bsize) {
		return 0, errors.New("用户文件系统类型或容量无效")
	}
	return int64(stat.Blocks) * stat.Bsize, nil
}
