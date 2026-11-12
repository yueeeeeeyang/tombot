//go:build !linux

package nodeapi

import (
	"context"
	"errors"
	"net"
)

type Listener struct{ *net.UnixListener }

func Listen(string, uint32) (*Listener, error) {
	return nil, errors.New("受信电脑节点仅支持 Linux")
}
func (*Listener) Close() error { return errors.New("受信电脑节点仅支持 Linux") }
func dialControl(context.Context, string) (*net.UnixConn, error) {
	return nil, errors.New("受信电脑节点仅支持 Linux")
}
