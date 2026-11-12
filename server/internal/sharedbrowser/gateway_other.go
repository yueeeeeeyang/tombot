//go:build !linux

package sharedbrowser

import "net"

func gatewayPeer(*net.UnixConn) (Peer, error) { return Peer{}, ErrUnavailable }
