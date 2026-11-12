//go:build linux

package egressproxy

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func readPeer(connection *net.UnixConn) (Peer, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return Peer{}, errors.New("读取出网内核连接失败")
	}
	var credential *unix.Ucred
	var inner error
	if err := raw.Control(func(fd uintptr) { credential, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || inner != nil || credential == nil {
		return Peer{}, errors.New("读取出网内核身份失败")
	}
	return Peer{PID: credential.Pid, UID: credential.Uid, GID: credential.Gid}, nil
}

func validateListener(listener *net.UnixListener) error {
	if os.Geteuid() != 0 {
		return errors.New("电脑出网监听要求受信 root 节点")
	}
	path := listener.Addr().String()
	if len(path) > 100 || protectedParents(path) != nil {
		return errors.New("电脑出网端点路径无效")
	}
	_, err := protectedSocket(path)
	return err
}
