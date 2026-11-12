package dockernode

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

type diskLayoutIdentity struct {
	rootUID, rootGID, sharedGID, browserUID int
}

// prepare 仅在首次格式化后创建固定子目录；已就绪磁盘缺失目录时拒绝补建，避免掩盖数据丢失。
// 根目录不暴露给业务进程，目录描述符和固定名称阻止链接替换；不遍历业务或浏览器内容。
func (identity diskLayoutIdentity) prepare(path string, create bool) (result error) {
	root, err := openLayoutDirectory(unix.AT_FDCWD, path)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if !layoutDirectoryMatches(root, identity.rootUID, identity.rootGID, 0755) {
		return failure("storage_layout_root_invalid")
	}
	names, err := root.Readdirnames(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return failure("storage_layout_read_failed")
	}
	for _, name := range names {
		switch name {
		case computer.WorkspaceSubdirectory, computer.BrowserSubdirectory:
		case "lost+found":
			if err = identity.child(root, name, identity.rootUID, identity.rootGID, 0700, false); err != nil {
				return err
			}
		default:
			return failure("storage_layout_has_foreign_files")
		}
	}
	if err = identity.child(root, computer.WorkspaceSubdirectory, identity.rootUID, identity.sharedGID, os.ModeSetgid|0770, create); err != nil {
		return err
	}
	if err = identity.child(root, computer.BrowserSubdirectory, identity.browserUID, identity.sharedGID, 0700, create); err != nil {
		return err
	}
	if create {
		if err = root.Sync(); err != nil {
			return failure("storage_layout_sync_failed")
		}
	}
	return nil
}

func openLayoutDirectory(parent int, name string) (*os.File, error) {
	descriptor, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, failure("storage_layout_directory_invalid")
	}
	return os.NewFile(uintptr(descriptor), name), nil
}

func layoutDirectoryMatches(directory *os.File, uid, gid int, mode os.FileMode) bool {
	metadata, err := directory.Stat()
	if err != nil {
		return false
	}
	identity, ok := metadata.Sys().(*syscall.Stat_t)
	return ok && metadata.IsDir() && identity.Uid == uint32(uid) && identity.Gid == uint32(gid) && metadata.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSetuid|os.ModeSticky) == mode
}

func (identity diskLayoutIdentity) child(root *os.File, name string, uid, gid int, mode os.FileMode, create bool) (result error) {
	if create {
		if err := unix.Mkdirat(int(root.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return failure("storage_layout_create_failed")
		}
	}
	directory, err := openLayoutDirectory(int(root.Fd()), name)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	if !layoutDirectoryMatches(directory, uid, gid, mode) {
		if !create || (!layoutDirectoryMatches(directory, identity.rootUID, identity.rootGID, 0700) && !layoutDirectoryMatches(directory, uid, gid, 0700)) {
			return failure("storage_layout_identity_mismatch")
		}
		// 授权中断只能恢复初始权限的空目录，不能递归修复已有数据或宽松权限。
		names, readErr := directory.Readdirnames(1)
		if len(names) != 0 || !errors.Is(readErr, io.EOF) {
			return failure("storage_layout_initial_directory_not_empty")
		}
		if err = directory.Chown(uid, gid); err != nil {
			return failure("storage_layout_owner_failed")
		}
		if err = directory.Chmod(mode); err != nil {
			return failure("storage_layout_permissions_failed")
		}
	}
	if !layoutDirectoryMatches(directory, uid, gid, mode) {
		return failure("storage_layout_identity_mismatch")
	}
	if create {
		if err = directory.Sync(); err != nil {
			return failure("storage_layout_sync_failed")
		}
	}
	return nil
}
