package nodecommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"io.yueyang/tom/internal/nodeenrollment"
)

const maximumEnrollmentInput = nodeenrollment.MaximumJoinCode

// readEnrollmentToken 从控制终端读取接入码；不接受命令参数或环境变量，避免进入历史及进程信息。
func readEnrollmentToken(ctx context.Context) (token []byte, result error) {
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("无法打开交互终端，请在终端运行 tomnode install 并粘贴接入凭据码")
	}
	defer func() { result = errors.Join(result, terminal.Close()) }()
	return readEnrollmentFromTerminal(ctx, terminal)
}

// readEnrollmentFromTerminal 隐藏输入并限制长度；超时、取消和读取失败都恢复终端并清理未完成输入。
func readEnrollmentFromTerminal(parent context.Context, terminal *os.File) (token []byte, result error) {
	return readTerminalLine(parent, terminal, "请粘贴管理平台生成的接入码并回车（输入不回显，Ctrl+C 取消）：", true, maximumEnrollmentInput)
}

func readTerminalLine(parent context.Context, terminal *os.File, prompt string, secret bool, maximum int) (token []byte, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd := int(terminal.Fd())
	original, err := getTerminalMode(fd)
	if err != nil {
		return nil, errors.New("接入凭据必须从交互终端输入")
	}
	hidden := *original
	if secret {
		hidden.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON
		hidden.Cc[unix.VMIN] = 1
		hidden.Cc[unix.VTIME] = 0
	}
	if !secret {
		hidden.Lflag |= unix.ICANON
	}
	if setTerminalMode(fd, &hidden) != nil {
		return nil, errors.New("无法隐藏终端输入，未读取凭据")
	}
	defer func() {
		if flushTerminalInput(fd) != nil {
			result = errors.Join(result, errors.New("清理终端输入失败"))
		}
		if setTerminalMode(fd, original) != nil {
			result = errors.Join(result, errors.New("恢复终端输入状态失败"))
		}
		if _, err := terminal.WriteString("\n"); err != nil {
			result = errors.Join(result, errors.New("终端提示输出失败"))
		}
		if result != nil {
			clear(token)
			token = nil
		}
	}()
	if _, err := terminal.WriteString(prompt); err != nil {
		return nil, errors.New("终端提示输出失败")
	}
	input := make([]byte, 0, maximum)
	defer func() { clear(input) }()
	chunk := make([]byte, maximum+1)
	defer clear(chunk[:])
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return nil, errors.New("终端连接中断，未保存接入凭据")
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		count, err := unix.Read(fd, chunk[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || count == 0 {
			return nil, errors.New("接入凭据输入已终止，未保存")
		}
		for _, character := range chunk[:count] {
			if character == '\n' || character == '\r' {
				return bytes.Clone(bytes.TrimSpace(input)), nil
			}
			if secret && character == 4 {
				return nil, errors.New("接入码输入已终止")
			}
			if secret && (character == 8 || character == 127) {
				if len(input) > 0 {
					input[len(input)-1] = 0
					input = input[:len(input)-1]
				}
				continue
			}
			if len(input) >= maximum {
				return nil, errors.New("终端输入过长，请重新复制")
			}
			input = append(input, character)
		}
	}
}

// readInstallationAddress 只读取公开的网卡或访问地址；空行表示接受展示的默认值。
func readInstallationAddress(ctx context.Context, prompt string) (value string, result error) {
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("无法打开安装交互终端")
	}
	defer func() { result = errors.Join(result, terminal.Close()) }()
	encoded, err := readTerminalLine(ctx, terminal, prompt, false, 512)
	if err != nil {
		return "", err
	}
	defer clear(encoded)
	return string(encoded), nil
}
