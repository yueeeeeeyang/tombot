//go:build !linux

package agentchannel

import (
	"errors"
	"time"
)

func FromDeadlineForProcess(time.Time, int) (Deadline, error) {
	return Deadline{}, errors.New("电脑代理目标时钟换算仅支持 Linux")
}

func monotonicClock() (int64, uint64, error) {
	return 0, 0, errors.New("电脑代理共享单调时钟仅支持 Linux")
}
