//go:build linux

package agentchannel

import (
	"errors"
	"golang.org/x/sys/unix"
)

func monotonicClock() (int64, uint64, error) {
	var stamp unix.Timespec
	var namespace unix.Stat_t
	if unix.Stat("/proc/self/ns/time", &namespace) != nil || namespace.Ino == 0 || unix.ClockGettime(unix.CLOCK_MONOTONIC, &stamp) != nil {
		return 0, 0, errors.New("读取电脑代理共享单调时钟失败")
	}
	if stamp.Sec < 0 || stamp.Nsec < 0 || stamp.Nsec >= 1000000000 || stamp.Sec > (1<<63-1-stamp.Nsec)/1000000000 {
		return 0, 0, errors.New("电脑代理共享单调时钟数值无效")
	}
	return stamp.Nano(), namespace.Ino, nil
}
