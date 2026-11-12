//go:build linux

package computeragent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// desktopDirectory 只管理一代执行的图形身份状态，不在共享卷上建立业务文件访问边界。
type desktopDirectory struct {
	mu         sync.Mutex
	root       *os.Root
	session    computer.ExecutionSession
	device     uint64
	inode      uint64
	closed     bool
	closeError error
}

// newDesktopDirectory 在授权尚未发给用户进程时建立完整目录；旧目录存在即拒绝，不复用认证状态。
func newDesktopDirectory(path string, session computer.ExecutionSession) (directory *desktopDirectory, failure error) {
	if err := session.Validate(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("打开电脑桌面运行根目录失败")
	}
	parent := os.NewFile(uintptr(fd), "desktop-runtime-root")
	defer func() {
		if err := parent.Close(); err != nil {
			failure = errors.Join(failure, errors.New("关闭桌面运行根目录描述符失败"))
			if directory != nil {
				failure = errors.Join(failure, directory.Close())
				directory = nil
			}
		}
	}()
	var identity unix.Stat_t
	if unix.Fstat(fd, &identity) != nil || identity.Uid != 0 || identity.Gid != 0 || identity.Mode&07777 != 0711 {
		return nil, errors.New("电脑桌面运行根目录身份或权限无效")
	}
	// 从已固定的目录描述符建立受限根，后续清理不会跟随业务进程放入目录内的符号链接。
	root, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, errors.New("固定电脑桌面运行根目录失败")
	}
	created := false
	defer func() {
		if directory == nil {
			if created {
				failure = errors.Join(failure, root.RemoveAll(session.SessionID))
			}
			failure = errors.Join(failure, root.Close())
		}
	}()
	if err = root.Mkdir(session.SessionID, 0700); err != nil {
		return nil, errors.New("创建独立桌面运行目录失败，不能覆盖旧会话")
	}
	created = true
	state, err := root.OpenRoot(session.SessionID)
	if err != nil {
		return nil, errors.New("打开新桌面运行目录失败")
	}
	if err = initializeDesktopDirectory(state, session); err != nil {
		return nil, errors.Join(err, state.Close())
	}
	if err = state.Close(); err != nil {
		return nil, errors.New("关闭新桌面运行目录失败")
	}
	metadata, err := root.Lstat(session.SessionID)
	if err != nil {
		return nil, errors.New("复验新桌面运行目录失败")
	}
	owner, ok := metadata.Sys().(*syscall.Stat_t)
	if !ok || !metadata.IsDir() || metadata.Mode().Perm() != 0700 || owner.Uid != uint32(session.UID) || owner.Gid != uint32(session.GID) {
		return nil, errors.New("新桌面运行目录未正确绑定身份")
	}
	return &desktopDirectory{root: root, session: session, device: uint64(owner.Dev), inode: owner.Ino}, nil
}

func initializeDesktopDirectory(root *os.Root, session computer.ExecutionSession) error {
	for _, name := range []string{"config", "cache", "state", "data", "tmp"} {
		if err := root.Mkdir(name, 0700); err != nil {
			return errors.New("准备桌面图形状态目录失败")
		}
		if err := root.Chown(name, session.UID, session.GID); err != nil {
			return errors.New("绑定桌面图形目录身份失败")
		}
		if err := root.Chmod(name, 0700); err != nil {
			return errors.New("设置桌面图形目录私有权限失败")
		}
	}
	for name, contents := range map[string]string{
		"passwd": fmt.Sprintf("bot%d:x:%d:%d:Bot:/workspace:/bin/bash\n", session.UID, session.UID, session.GID),
		"group":  fmt.Sprintf("bot:x:%d:\n", session.GID),
	} {
		if err := writeDesktopIdentity(root, name, contents, session); err != nil {
			return err
		}
	}
	// 所有身份文件准备完成后才把父目录交给 Bot，避免其他同 UID 进程看到半初始化状态。
	if err := root.Chmod(".", 0700); err != nil {
		return errors.New("设置桌面运行目录私有权限失败")
	}
	if err := root.Chown(".", session.UID, session.GID); err != nil {
		return errors.New("绑定桌面运行目录身份失败")
	}
	return nil
}

func writeDesktopIdentity(root *os.Root, name, contents string, session computer.ExecutionSession) (failure error) {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("创建桌面身份文件失败")
	}
	defer func() { failure = errors.Join(failure, file.Close()) }()
	if count, err := file.WriteString(contents); err != nil || count != len(contents) {
		return errors.New("写入桌面身份文件失败")
	}
	if err = file.Chown(session.UID, session.GID); err != nil {
		return errors.New("绑定桌面身份文件所有者失败")
	}
	if err = file.Chmod(0600); err != nil {
		return errors.New("设置桌面身份文件私有权限失败")
	}
	return nil
}

// Close 只允许调用者确认 cgroup 为空后执行；目录缺失可视为已清理，替换为其他对象则拒绝。
func (directory *desktopDirectory) Close() error {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.closed {
		return directory.closeError
	}
	identity, err := directory.root.Lstat(directory.session.SessionID)
	if err == nil {
		owner, ok := identity.Sys().(*syscall.Stat_t)
		if !ok || !identity.IsDir() || uint64(owner.Dev) != directory.device || owner.Ino != directory.inode {
			return errors.New("待清理桌面目录身份被替换")
		}
		if err = directory.root.RemoveAll(directory.session.SessionID); err != nil {
			return errors.New("清理已停止桌面运行目录失败")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("检查待清理桌面运行目录失败")
	}
	directory.closed = true
	directory.closeError = directory.root.Close()
	return directory.closeError
}
