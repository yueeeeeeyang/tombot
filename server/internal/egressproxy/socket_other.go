//go:build !linux

package egressproxy

import (
	"errors"
	"net"
)

type Listener struct{ *net.UnixListener }

func Listen(string) (*Listener, error) {
	return nil, errors.New("电脑出网端点要求 Linux 受信节点")
}

func (*Listener) Check() error { return errors.New("电脑出网端点要求 Linux 受信节点") }
