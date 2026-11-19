//go:build linux

package taskprocess

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// runDesktopChrome 只在已入组的受信子进程执行；工作区的环境和程序路径不会带入浏览器身份。
func runDesktopChrome(session computer.ExecutionSession, revocation *os.File) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	uid := computer.DesktopBrowserUIDBase + session.DisplayNumber
	if err := privateBrowserDirectory(computer.BrowserDirectory, session.DesktopID, uid, 0700, true); err != nil {
		return err
	}
	if err := privateBrowserDirectory(computer.BrowserRuntimeRoot, session.SessionID, uid, 0711, false); err != nil {
		return err
	}
	if err := prepareProcess(uid, session.GID); err != nil {
		return err
	}
	if err := checkRevocation(revocation); err != nil {
		return err
	}
	return syscall.Exec("/usr/bin/python3", []string{"python3", "-I", "-B", "/usr/lib/tombot/open_browser.py", "serve", session.SessionID, session.DesktopID, strconv.Itoa(session.DisplayNumber)}, []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"})
}

// privateBrowserDirectory 只在 root 独占写权限的父目录内创建，拒绝链接、其他桌面和异常权限。
func privateBrowserDirectory(parent, name string, uid int, mode uint32, persistent bool) (failure error) {
	fd, err := unix.Open(parent, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, unix.Close(fd)) }()
	var identity unix.Stat_t
	if unix.Fstat(fd, &identity) != nil || identity.Uid != 0 || identity.Gid != 0 || identity.Mode&07777 != 0711 {
		return errors.New("桌面浏览器根目录身份无效")
	}
	created := true
	if err = unix.Mkdirat(fd, name, 0700); err != nil {
		if !persistent || !errors.Is(err, unix.EEXIST) {
			return err
		}
		created = false
	}
	child, err := unix.Openat(fd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(child), name)
	defer func() { failure = errors.Join(failure, directory.Close()) }()
	if err = unix.Fstat(child, &identity); err != nil {
		return err
	}
	// 若在 mkdir 与 chown 之间异常退出，只允许继续授权未写入数据的 root 私有目录。
	if !created && persistent && identity.Uid == 0 && identity.Gid == 0 && identity.Mode&07777 == 0700 {
		names, readErr := directory.Readdirnames(1)
		if len(names) != 0 || !errors.Is(readErr, io.EOF) {
			return errors.New("中断初始化的浏览器配置目录非空或无法核验")
		}
		created = true
	}
	if created {
		if err = unix.Fchmod(child, mode); err != nil {
			return err
		}
		if err = unix.Fchown(child, uid, computer.DesktopSharedGID); err != nil {
			return err
		}
		if err = directory.Sync(); err != nil {
			return err
		}
		if err = unix.Fsync(fd); err != nil {
			return err
		}
	}
	if unix.Fstat(child, &identity) != nil || identity.Uid != uint32(uid) || identity.Gid != computer.DesktopSharedGID || identity.Mode&07777 != mode {
		return errors.New("桌面浏览器目录身份无效")
	}
	return nil
}
