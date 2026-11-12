//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type privateStream struct{ input, output *os.File }

func (stream *privateStream) Read(buffer []byte) (int, error)  { return stream.input.Read(buffer) }
func (stream *privateStream) Write(buffer []byte) (int, error) { return stream.output.Write(buffer) }
func (stream *privateStream) Close() error {
	return errors.Join(stream.input.Close(), stream.output.Close())
}

// streamFile 将继承的阻塞管道纳入 Go 轮询器，否则关闭标准流不能可靠打断阻塞系统调用。
// 只接收私有管道或套接字，复制后的描述符仅由当前 Agent 关闭。
func streamFile(descriptor int, name string) (*os.File, error) {
	var status unix.Stat_t
	if err := unix.Fstat(descriptor, &status); err != nil {
		return nil, err
	}
	if kind := status.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		return nil, errors.New("标准流必须是私有管道或套接字")
	}
	copyDescriptor, err := unix.FcntlInt(uintptr(descriptor), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	if err = unix.SetNonblock(copyDescriptor, true); err != nil {
		return nil, errors.Join(err, unix.Close(copyDescriptor))
	}
	file := os.NewFile(uintptr(copyDescriptor), name)
	if err = file.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func standardStream() (io.ReadWriteCloser, error) {
	input, err := streamFile(0, "bot-agent-input")
	if err != nil {
		return nil, err
	}
	output, err := streamFile(1, "bot-agent-output")
	if err != nil {
		return nil, errors.Join(err, input.Close())
	}
	return &privateStream{input: input, output: output}, nil
}
