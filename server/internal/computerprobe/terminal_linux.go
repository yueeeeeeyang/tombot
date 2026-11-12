//go:build linux

package computerprobe

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func checkTerminal(ctx context.Context) (failure error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	master, slave, err := pty.Open()
	if err != nil {
		return errors.New("能力探针无法建立伪终端")
	}
	defer func() { failure = errors.Join(failure, master.Close(), slave.Close()) }()
	masterFD, slaveFD := int(master.Fd()), int(slave.Fd())
	if pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}) != nil {
		return errors.New("能力探针无法调整伪终端")
	}
	size, err := pty.GetsizeFull(slave)
	if err != nil || size.Rows != 24 || size.Cols != 80 {
		return errors.New("能力探针伪终端尺寸未生效")
	}
	settings, err := unix.IoctlGetTermios(slaveFD, unix.TCGETS)
	if err != nil {
		return errors.New("能力探针无法读取终端模式")
	}
	// 关闭行缓冲和回显，双向数据必须来自实际终端另一端，不能把本地回显当作成功。
	settings.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	settings.Oflag &^= unix.OPOST
	settings.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	settings.Cflag &^= unix.CSIZE | unix.PARENB
	settings.Cflag |= unix.CS8
	settings.Cc[unix.VMIN], settings.Cc[unix.VTIME] = 1, 0
	if unix.IoctlSetTermios(slaveFD, unix.TCSETS, settings) != nil || unix.SetNonblock(masterFD, true) != nil || unix.SetNonblock(slaveFD, true) != nil {
		return errors.New("能力探针无法配置非阻塞终端")
	}
	for _, direction := range []struct {
		writer, reader int
		content        string
	}{{masterFD, slaveFD, "终端输入\x00\n"}, {slaveFD, masterFD, "终端输出\x00\n"}} {
		if err := transfer(ctx, direction.writer, direction.reader, []byte(direction.content)); err != nil {
			return errors.Join(errors.New("能力探针终端双向读写失败"), err)
		}
	}
	return ctx.Err()
}

func waitDescriptor(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: events}}
		count, err := unix.Poll(poll, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return errors.New("能力探针终端描述符不可用")
		}
		if count > 0 && poll[0].Revents&events != 0 {
			return ctx.Err()
		}
	}
}

func transfer(ctx context.Context, writer, reader int, content []byte) error {
	for offset := 0; offset < len(content); {
		if err := waitDescriptor(ctx, writer, unix.POLLOUT); err != nil {
			return err
		}
		count, err := unix.Write(writer, content[offset:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || count <= 0 {
			return errors.New("写入能力探针终端失败")
		}
		offset += count
	}
	actual := make([]byte, len(content))
	for offset := 0; offset < len(actual); {
		if err := waitDescriptor(ctx, reader, unix.POLLIN); err != nil {
			return err
		}
		count, err := unix.Read(reader, actual[offset:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || count <= 0 {
			return io.ErrUnexpectedEOF
		}
		offset += count
	}
	if !bytes.Equal(actual, content) {
		return errors.New("能力探针终端内容不符")
	}
	return nil
}
