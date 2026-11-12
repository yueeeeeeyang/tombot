//go:build !linux

package agentchannel

import "errors"

func monotonicClock() (int64, uint64, error) {
	return 0, 0, errors.New("电脑代理共享单调时钟仅支持 Linux")
}
