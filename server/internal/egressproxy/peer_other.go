//go:build !linux

package egressproxy

import (
	"errors"
	"net"
)

func readPeer(*net.UnixConn) (Peer, error) {
	return Peer{}, errors.New("电脑出网通道仅支持 Linux")
}
func validateListener(*net.UnixListener) error {
	return errors.New("电脑出网通道仅支持 Linux")
}
