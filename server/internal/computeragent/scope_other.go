//go:build !linux

package computeragent

import (
	"errors"
	"time"
)

func NewKernelScopes(time.Duration, WindowFactory) (ScopeFactory, error) {
	return nil, errors.New("电脑代理资源组后端仅支持 Linux")
}
