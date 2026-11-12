//go:build !linux

package processgroup

import (
	"errors"
	"log/slog"
	"os"
)

// NewServices 在非 Linux 平台明确拒绝，不用宿主普通进程模拟服务资源组。
func NewServices(Options, *slog.Logger) (ServiceGroups, error) {
	return nil, errors.New("容器基础服务进程组要求 Linux cgroup v2")
}

func ValidateServiceControls(ServiceReference, []*os.File) error {
	return errors.New("基础服务控制描述符仅支持 Linux")
}
