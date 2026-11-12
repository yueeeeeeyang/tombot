//go:build !linux

package agentchannel

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

type Peer struct {
	PID      int32
	UID, GID uint32
}
type Channel struct{}

func New(*net.UnixConn, time.Duration) (*Channel, Peer, error) {
	return nil, Peer{}, errors.New("电脑代理控制通道仅支持 Linux")
}
func (*Channel) Close() error { return errors.New("电脑代理控制通道仅支持 Linux") }
func (*Channel) Send(context.Context, Message, []*os.File) error {
	return errors.New("电脑代理控制通道仅支持 Linux")
}
func (*Channel) Receive(context.Context) (Message, []*os.File, error) {
	return Message{}, nil, errors.New("电脑代理控制通道仅支持 Linux")
}
