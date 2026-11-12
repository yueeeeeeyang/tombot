package term

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

const processInspectionTimeout = 2 * time.Second

// signalSession 按 SID 查找所有作业组，避免交互式 Shell 的作业控制使后台进程漏清理。
// 主动 setsid 脱离会话的程序不属于 PTY 生命周期；这里不提供操作系统级隔离。
func signalSession(sessionID int, signal syscall.Signal) error {
	ctx, cancel := context.WithTimeout(context.Background(), processInspectionTimeout)
	defer cancel()
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return errors.Join(err, signalGroup(sessionID, signal))
	}
	groups := map[int]bool{sessionID: true}
	var failures error
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			failures = errors.Join(failures, err)
			break
		}
		sid, err := unix.Getsid(int(pid))
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("读取进程 %d 会话失败: %w", pid, err))
			continue
		}
		if sid != sessionID {
			continue
		}
		group, err := unix.Getpgid(int(pid))
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if group > 1 {
			groups[group] = true
		}
	}
	for group := range groups {
		failures = errors.Join(failures, signalGroup(group, signal))
	}
	return failures
}
