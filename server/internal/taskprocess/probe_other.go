//go:build !linux

package taskprocess

import (
	"errors"
	"os"
	"time"
)

func SealProbe(ProbeRequest) (*os.File, error) {
	return nil, errors.New("能力探针封存仅支持 Linux")
}

func RunProbe() error { return errors.New("能力探针引导仅支持受管 Linux 容器") }

func CheckProbe(ProbeRequest) (time.Time, error) {
	return time.Time{}, errors.New("能力探针执行仅支持受管 Linux 容器")
}
