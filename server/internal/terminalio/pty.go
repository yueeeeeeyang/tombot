package terminalio

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"time"
)

// PollablePTY 在启动阶段复制非阻塞描述符，让 Go 轮询器管理读写超时和关闭唤醒。
// 后续禁止调用 File.Fd，避免它把描述符重新切换到阻塞模式。
func PollablePTY(master *os.File) (*os.File, error) {
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

func ResizePTY(file *os.File, columns, rows int) error {
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
