//go:build linux

package egressproxy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Listener 在受保护目录持有稳定独占锁，只有确认原 socket 身份后才清理陈旧端点。
type Listener struct {
	*net.UnixListener
	lock       *os.File
	path       string
	identity   os.FileInfo
	closeOnce  sync.Once
	closeError error
}

// Check 复验监听路径仍指向本实例的私有 socket，路径被替换时不能继续宣告端点可用。
func (listener *Listener) Check() error {
	if protectedParents(listener.path) != nil {
		return errors.New("电脑出网父目录已改变")
	}
	current, err := protectedSocket(listener.path)
	if err != nil || !os.SameFile(listener.identity, current) {
		return errors.New("电脑出网端点身份已改变")
	}
	return nil
}

func protectedParents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("电脑出网 socket 必须是规范绝对路径")
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return errors.New("电脑出网 socket 父目录不可用")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("电脑出网 socket 父目录未受 root 保护")
		}
		if directory == "/" {
			break
		}
	}
	return nil
}

func protectedSocket(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return nil, errors.New("电脑出网 socket 身份或权限无效")
	}
	return info, nil
}

// Listen 不自动创建配置目录，也不删除普通文件、链接或其他身份的 socket；重复服务启动明确失败。
func Listen(path string) (listener *Listener, failure error) {
	if os.Geteuid() != 0 || path == "/" || len(path) > 100 || protectedParents(path) != nil {
		return nil, errors.New("电脑出网监听要求 root 和受保护的端点目录")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("打开电脑出网监听锁失败")
	}
	defer func() {
		if listener == nil {
			failure = errors.Join(failure, lock.Close())
		}
	}()
	info, err := lock.Stat()
	if err != nil {
		return nil, errors.New("读取电脑出网监听锁失败")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("电脑出网监听锁身份或权限无效")
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, errors.New("电脑出网监听已由其他进程持有")
	}
	_, err = protectedSocket(path)
	if err == nil {
		connection, probeErr := net.DialTimeout("unix", path, 100*time.Millisecond)
		if probeErr == nil {
			return nil, errors.Join(errors.New("电脑出网端点仍有监听者"), connection.Close())
		}
		if !errors.Is(probeErr, syscall.ECONNREFUSED) {
			return nil, errors.New("旧电脑出网端点是否已停止尚未确认")
		}
		if os.Remove(path) != nil {
			return nil, errors.New("清理已失去监听者的电脑出网端点失败")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("创建电脑出网监听失败")
	}
	unixListener.SetUnlinkOnClose(false)
	created, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Join(errors.New("读取新电脑出网端点身份失败"), unixListener.Close())
	}
	defer func() {
		if listener != nil {
			return
		}
		failure = errors.Join(failure, unixListener.Close())
		current, err := os.Lstat(path)
		if err == nil && os.SameFile(created, current) {
			failure = errors.Join(failure, os.Remove(path))
		} else if !errors.Is(err, os.ErrNotExist) {
			failure = errors.Join(failure, errors.New("失败的电脑出网端点身份未确认，未删除"))
		}
	}()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, errors.New("设置电脑出网端点权限失败")
	}
	identity, err := protectedSocket(path)
	if err != nil {
		return nil, err
	}
	return &Listener{UnixListener: unixListener, lock: lock, path: path, identity: identity}, nil
}

// Close 不删除稳定锁文件；若端点已被替换，保留未知文件并明确报告，避免误删其他监听者。
func (listener *Listener) Close() error {
	listener.closeOnce.Do(func() {
		if err := listener.UnixListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			listener.closeError = errors.New("关闭电脑出网监听失败")
		}
		info, err := os.Lstat(listener.path)
		if err == nil {
			if !os.SameFile(listener.identity, info) {
				listener.closeError = errors.Join(listener.closeError, errors.New("电脑出网端点身份已变更，未清理"))
			} else if os.Remove(listener.path) != nil {
				listener.closeError = errors.Join(listener.closeError, errors.New("删除电脑出网端点失败"))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			listener.closeError = errors.Join(listener.closeError, errors.New("检查电脑出网端点失败"))
		}
		listener.closeError = errors.Join(listener.closeError, listener.lock.Close())
	})
	return listener.closeError
}
