package nodeapp

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"time"
)

// NotifyReady 通知 systemd 控制服务已启动；没有配置通知端点时允许前台运行，不发布电脑健康状态。
func NotifyReady(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	if len(path) > 100 || strings.ContainsAny(path, "\x00\r\n") || (!strings.HasPrefix(path, "@") && (!filepath.IsAbs(path) || filepath.Clean(path) != path)) || path == "@" {
		return errors.New("节点启动通知端点无效")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unixgram", path)
	if err != nil {
		return errors.New("节点启动通知连接失败")
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetWriteDeadline(deadline); err != nil {
		return errors.Join(errors.New("节点启动通知时限设置失败"), connection.Close())
	}
	message := []byte("READY=1\nSTATUS=节点控制服务已启动，用户电脑仍需完整能力检查\n")
	count, writeErr := connection.Write(message)
	closeErr := connection.Close()
	if writeErr != nil || closeErr != nil || count != len(message) {
		return errors.New("节点启动通知未确认送达")
	}
	return ctx.Err()
}
