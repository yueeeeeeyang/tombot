//go:build !linux

package computeragent

import (
	"errors"
	"log/slog"
	"time"
)

func NewKernelScopes(time.Duration, WindowFactory, *slog.Logger) (ScopeFactory, error) {
	return nil, errors.New("电脑代理资源组后端仅支持 Linux")
}
