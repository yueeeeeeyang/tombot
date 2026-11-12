//go:build linux

package nodeapi

import (
	"context"
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

func protectedParents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("节点 socket 必须是规范绝对路径")
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return errors.New("节点 socket 父目录不可用")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("节点 socket 父目录未受 root 保护")
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
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0660 {
		return nil, errors.New("节点 socket 身份或权限无效")
	}
	return info, nil
}

// Listen 不自动创建配置目录，也不删除普通文件、链接或其他身份的 socket；重复服务启动明确失败。
func Listen(path string, clientGID uint32) (listener *Listener, failure error) {
	if os.Geteuid() != 0 || clientGID == 0 || clientGID == ^uint32(0) || path == "/" || len(path) > 100 || protectedParents(path) != nil {
		return nil, errors.New("节点监听要求 root 和受保护的端点目录")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("打开节点监听锁失败")
	}
	defer func() {
		if listener == nil {
			failure = errors.Join(failure, lock.Close())
		}
	}()
	info, err := lock.Stat()
	if err != nil {
		return nil, errors.New("读取节点监听锁失败")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("节点监听锁身份或权限无效")
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, errors.New("节点监听已由其他进程持有")
	}
	old, err := protectedSocket(path)
	if err == nil {
		if old.Sys().(*syscall.Stat_t).Gid != clientGID {
			return nil, errors.New("旧节点端点的控制组不符")
		}
		connection, probeErr := net.DialTimeout("unixpacket", path, 100*time.Millisecond)
		if probeErr == nil {
			return nil, errors.Join(errors.New("节点端点仍有监听者"), connection.Close())
		}
		if !errors.Is(probeErr, syscall.ECONNREFUSED) {
			return nil, errors.New("旧节点端点是否已停止尚未确认")
		}
		if os.Remove(path) != nil {
			return nil, errors.New("清理已失去监听者的节点端点失败")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	unixListener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, errors.New("创建节点监听失败")
	}
	unixListener.SetUnlinkOnClose(false)
	created, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Join(errors.New("读取新节点端点身份失败"), unixListener.Close())
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
			failure = errors.Join(failure, errors.New("失败的节点端点身份未确认，未删除"))
		}
	}()
	if err := os.Chown(path, 0, int(clientGID)); err != nil {
		return nil, errors.New("设置节点端点所属组失败")
	}
	if err := os.Chmod(path, 0660); err != nil {
		return nil, errors.New("设置节点端点权限失败")
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
			listener.closeError = errors.New("关闭节点监听失败")
		}
		info, err := os.Lstat(listener.path)
		if err == nil {
			if !os.SameFile(listener.identity, info) {
				listener.closeError = errors.Join(listener.closeError, errors.New("节点端点身份已变更，未清理"))
			} else if os.Remove(listener.path) != nil {
				listener.closeError = errors.Join(listener.closeError, errors.New("删除节点端点失败"))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			listener.closeError = errors.Join(listener.closeError, errors.New("检查节点端点失败"))
		}
		listener.closeError = errors.Join(listener.closeError, listener.lock.Close())
	})
	return listener.closeError
}

func dialControl(ctx context.Context, path string) (*net.UnixConn, error) {
	if err := protectedParents(path); err != nil {
		return nil, err
	}
	if _, err := protectedSocket(path); err != nil {
		return nil, errors.New("节点端点不可用或身份无效")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", path)
	if err != nil {
		return nil, errors.New("连接节点端点失败")
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, errors.Join(errors.New("节点连接类型无效"), connection.Close())
	}
	return unixConnection, nil
}
