package term

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const processZombie = 5

// XNU 对仅剩僵尸的进程组也可能返回 EPERM；只有系统快照确认没有活成员才视为已退出。
func normalizeGroupSignalError(pid int, err error) error {
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	const observations = 5
	for attempt := range observations {
		members, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
		if queryErr != nil {
			return errors.Join(err, queryErr)
		}
		hasLiveMember := false
		for _, member := range members {
			if member.Proc.P_stat != processZombie {
				hasLiveMember = true
				break
			}
		}
		if !hasLiveMember {
			return nil
		}
		if attempt < observations-1 {
			// 退出状态可稍晚于信号权限检查更新；只短暂观察，不重发可能已失去归属的信号。
			time.Sleep(time.Duration(1<<attempt) * time.Millisecond)
		}
	}
	return err
}
