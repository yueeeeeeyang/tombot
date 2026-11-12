//go:build linux

package sharedbrowser

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/computer"
)

// dialLocalBrowser 由降权后的 Agent 调用，固定目录描述符并核验 root 对端，拒绝路径替换和其他 UID 的伪造服务。
func dialLocalBrowser(ctx context.Context, session computer.ExecutionSession) (_ net.Conn, failure error) {
	var connection net.Conn
	defer func() {
		if failure != nil && connection != nil {
			failure = errors.Join(failure, connection.Close())
		}
	}()
	if session.Validate() != nil || os.Geteuid() != session.UID || os.Getegid() != session.GID {
		return nil, ErrDenied
	}
	root, err := unix.Open(computer.DesktopRuntimeRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { failure = errors.Join(failure, unix.Close(root)) }()
	var stat unix.Stat_t
	if unix.Fstat(root, &stat) != nil || stat.Uid != 0 || stat.Gid != 0 || stat.Mode&07777 != 0711 {
		return nil, ErrDenied
	}
	directory, err := unix.Openat2(root, session.SessionID, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { failure = errors.Join(failure, unix.Close(directory)) }()
	if unix.Fstat(directory, &stat) != nil || stat.Uid != uint32(session.UID) || stat.Gid != uint32(session.GID) || stat.Mode&07777 != 0700 {
		return nil, ErrDenied
	}
	if unix.Fstatat(directory, "browser.sock", &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != 0 || stat.Gid != uint32(session.GID) || stat.Mode&07777 != 0660 {
		return nil, ErrDenied
	}
	connection, err = (&net.Dialer{}).DialContext(ctx, "unix", "/proc/self/fd/"+strconv.Itoa(directory)+"/browser.sock")
	if err != nil {
		return nil, ErrUnavailable
	}
	peer, err := gatewayPeer(connection.(*net.UnixConn))
	if err != nil || peer.PID < 1 || peer.UID != 0 || peer.GID != 0 {
		return nil, ErrDenied
	}
	return connection, nil
}
