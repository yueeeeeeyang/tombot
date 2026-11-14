package servercommand

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// confirmAddress 读取公开访问地址，超时或终端中断明确取消，不把缺失输入当作确认。
func confirmAddress(parent context.Context, suggestion string) (address string, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("无交互终端，请用 --url 指定访问地址")
	}
	defer func() { result = errors.Join(result, terminal.Close()) }()
	return readAddress(ctx, terminal, suggestion)
}

// readAddress 使用固定非阻塞描述符，避免终端关闭或取消时挂在读取调用中。
func readAddress(ctx context.Context, terminal *os.File, suggestion string) (string, error) {
	fd := int(terminal.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return "", errors.New("无法设置终端读取方式")
	}
	if _, err := terminal.WriteString("请确认浏览器和电脑节点可访问的 HTTPS 地址 [" + suggestion + "]："); err != nil {
		return "", errors.New("终端提示写入失败")
	}
	var input []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.New("地址确认已取消或超时")
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return "", errors.New("地址确认终端已断开")
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		chunk := make([]byte, 513)
		count, err := unix.Read(fd, chunk)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || count == 0 {
			return "", errors.New("地址输入已终止")
		}
		for _, character := range chunk[:count] {
			if character == '\n' || character == '\r' {
				address := strings.TrimSpace(string(input))
				if address == "" {
					address = suggestion
				}
				return address, nil
			}
			if len(input) >= 512 {
				return "", errors.New("地址输入过长")
			}
			input = append(input, character)
		}
	}
}
