//go:build linux

package computeragent

import (
	"errors"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/taskprocess"
)

// browserDirectory 的独占锁属于 root 运行根目录，Chrome 无权删除或替换锁文件。
// 只有启动已结束且整个服务组已确认清空后，才能释放锁与本次临时目录。
type browserDirectory struct {
	root       *os.Root
	lock       *os.File
	name       string
	device     uint64
	inode      uint64
	closed     bool
	closeError error
}

func newBrowserDirectory(request taskprocess.BrowserRequest) (directory *browserDirectory, failure error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(computer.BrowserRuntimeRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("打开共享浏览器运行根目录失败")
	}
	parent := os.NewFile(uintptr(fd), "browser-runtime-root")
	defer func() {
		if err := parent.Close(); err != nil {
			failure = errors.Join(failure, err)
			if directory != nil {
				failure = errors.Join(failure, directory.Close())
				directory = nil
			}
		}
	}()
	var identity unix.Stat_t
	if unix.Fstat(fd, &identity) != nil || identity.Uid != 0 || identity.Gid != 0 || identity.Mode&07777 != 0711 {
		return nil, errors.New("共享浏览器运行根目录身份或权限无效")
	}
	root, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, errors.New("固定共享浏览器运行根目录失败")
	}
	lockFD, err := unix.Openat(fd, "active.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.Join(errors.New("打开共享浏览器独占锁失败"), root.Close())
	}
	lock := os.NewFile(uintptr(lockFD), "browser-lock")
	if unix.Fstat(lockFD, &identity) != nil || identity.Mode&unix.S_IFMT != unix.S_IFREG || identity.Uid != 0 || identity.Gid != 0 || identity.Mode&07777 != 0600 || identity.Nlink != 1 {
		return nil, errors.Join(errors.New("共享浏览器独占锁身份无效"), lock.Close(), root.Close())
	}
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(errors.New("共享浏览器已有活动实例或锁不可用"), lock.Close(), root.Close())
	}
	created := false
	defer func() {
		if directory == nil {
			if created {
				failure = errors.Join(failure, root.Remove(request.Group.ID))
			}
			failure = errors.Join(failure, lock.Close(), root.Close())
		}
	}()
	if err = root.Mkdir(request.Group.ID, 0700); err != nil {
		return nil, errors.New("共享浏览器临时实例目录已存在或不能创建")
	}
	created = true
	if err = root.Chown(request.Group.ID, computer.BrowserUID, computer.DesktopSharedGID); err != nil {
		return nil, errors.New("设置共享浏览器临时目录身份失败")
	}
	metadata, err := root.Lstat(request.Group.ID)
	if err != nil {
		return nil, errors.New("复验共享浏览器临时目录失败")
	}
	actual, ok := metadata.Sys().(*syscall.Stat_t)
	if !ok || !metadata.IsDir() || metadata.Mode().Perm() != 0700 || actual.Uid != computer.BrowserUID || actual.Gid != computer.DesktopSharedGID {
		return nil, errors.New("共享浏览器临时目录权限未保留")
	}
	return &browserDirectory{root: root, lock: lock, name: request.Group.ID, device: uint64(actual.Dev), inode: actual.Ino}, nil
}

func (directory *browserDirectory) Close() error {
	if directory.closed {
		return directory.closeError
	}
	metadata, err := directory.root.Lstat(directory.name)
	if err != nil {
		return errors.New("共享浏览器临时目录丢失，拒绝清理其他实例")
	}
	actual, ok := metadata.Sys().(*syscall.Stat_t)
	if !ok || !metadata.IsDir() || uint64(actual.Dev) != directory.device || actual.Ino != directory.inode {
		return errors.New("共享浏览器临时目录被替换")
	}
	if err = directory.root.RemoveAll(directory.name); err != nil {
		return errors.New("清理共享浏览器临时目录失败")
	}
	directory.closed = true
	directory.closeError = errors.Join(directory.lock.Close(), directory.root.Close())
	return directory.closeError
}
