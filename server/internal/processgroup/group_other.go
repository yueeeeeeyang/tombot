//go:build !linux

package processgroup

import (
	"context"
	"errors"
	"log/slog"
)

func LocateContainer(context.Context, string, int) (ContainerReference, error) {
	return ContainerReference{}, errors.New("容器身份核验要求 Linux cgroup v2")
}

// New 在非 Linux 系统明确失败，不使用宿主进程组模拟 cgroup 回收。
func New(Options, *slog.Logger) (Groups, error) {
	return nil, errors.New("电脑进程回收要求 Linux cgroup v2")
}
