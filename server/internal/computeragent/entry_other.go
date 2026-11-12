//go:build !linux

package computeragent

import (
	"context"
	"errors"
	"log/slog"
)

func Run(context.Context, *slog.Logger) error {
	return errors.New("电脑代理入口仅支持受管 Linux 容器")
}
