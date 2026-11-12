//go:build linux

package sharedbrowser

import (
	"net"

	"golang.org/x/sys/unix"
)

func gatewayPeer(connection *net.UnixConn) (Peer, error) {
	control, err := connection.SyscallConn()
	if err != nil {
		return Peer{}, ErrDenied
	}
	var credential *unix.Ucred
	var failure error
	if control.Control(func(fd uintptr) {
		credential, failure = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || failure != nil || credential == nil || credential.Pid < 1 {
		return Peer{}, ErrDenied
	}
	return Peer{PID: int(credential.Pid), UID: credential.Uid, GID: credential.Gid}, nil
}
