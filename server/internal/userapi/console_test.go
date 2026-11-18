package userapi

import (
	"context"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/identity"
	"net"
)

// 目录接口测试没有节点，意外访问桌面必须失败。
type unavailableConsole struct{}

func (unavailableConsole) Ready(context.Context, identity.Principal, string) error {
	return execution.ErrUnavailable
}
func (unavailableConsole) Stream(context.Context, identity.Principal, string) (net.Conn, error) {
	return nil, execution.ErrUnavailable
}
