//go:build linux

package taskprocess

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// verifyDesktopDirectory 只核验私有桌面控制目录；业务工作目录在降权后按正常文件权限访问。
func verifyDesktopDirectory(request Request) error {
	fd, err := unix.Open(computer.DesktopRuntimeRoot, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_RDONLY, 0)
	if err != nil {
		return errors.New("打开受管桌面根目录失败")
	}
	return verifyDesktopDirectoryAt(request, os.NewFile(uintptr(fd), "desktop-runtime-root"))
}

// verifyDesktopDirectoryAt 接管根描述符，拒绝其他身份、可替换根目录和链接；不创建或修复目录。
func verifyDesktopDirectoryAt(request Request, root *os.File) (failure error) {
	defer func() { failure = errors.Join(failure, root.Close()) }()
	if err := request.Validate(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if unix.Fstat(int(root.Fd()), &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || stat.Gid != 0 || stat.Mode&07777 != 0711 {
		return errors.New("桌面运行根目录所属身份或权限无效")
	}
	identity := request.Group.Session
	fd, err := unix.Openat2(int(root.Fd()), identity.SessionID, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return errors.New("打开桌面运行目录失败或目录包含链接")
	}
	directory := os.NewFile(uintptr(fd), "desktop-runtime")
	defer func() { failure = errors.Join(failure, directory.Close()) }()
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(identity.UID) || stat.Gid != uint32(identity.GID) || stat.Mode&07777 != 0700 {
		return errors.New("桌面运行目录所属身份或权限无效")
	}
	return nil
}
