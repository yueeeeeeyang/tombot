package term

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// pollablePTY 在启动阶段复制非阻塞描述符，让 Go 轮询器管理读写超时和关闭唤醒。
// 后续禁止调用 File.Fd，避免它把描述符重新切换到阻塞模式。
func pollablePTY(master *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, errors.Join(err, master.Close())
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		return nil, errors.Join(err, unix.Close(fd), master.Close())
	}
	file := os.NewFile(uintptr(fd), master.Name())
	if err = master.Close(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = file.SetWriteDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func resizePTY(file *os.File, columns, rows int) error {
	connection, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	err = connection.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(columns), Row: uint16(rows)})
	})
	return errors.Join(err, ioctlErr)
}
