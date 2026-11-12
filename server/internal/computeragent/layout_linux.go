//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/computer"
)

type preparationOutput struct{ buffer bytes.Buffer }

func (output *preparationOutput) Write(value []byte) (int, error) {
	if output.buffer.Len()+len(value) > 2048 {
		return 0, errors.New("运行空间准备输出超过限制")
	}
	return output.buffer.Write(value)
}

// prepareFilesystem 仅在持有代理独占锁且尚未接单时运行镜像内固定程序。
// 在独立子进程切换公共组，保证 setgid 目录授权，不改变多线程代理身份或增加 FSETID 权限。
func prepareFilesystem(ctx context.Context, timeout time.Duration) error {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(bounded, "/usr/bin/python3", "-I", "-B", "/usr/lib/tombot/prepare.py")
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 0, Gid: computer.DesktopSharedGID}}
	output := &preparationOutput{}
	command.Stdout, command.Stderr = io.Discard, output
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if bounded.Err() != nil {
			return errors.Join(errors.New("电脑运行空间准备超时或取消"), bounded.Err())
		}
		message := strings.TrimSpace(output.buffer.String())
		// 固定程序只输出常量诊断或异常类型；不返回 Python traceback、用户路径或业务内容。
		if strings.HasPrefix(message, "电脑运行空间准备失败：") && len(message) <= 512 && utf8.ValidString(message) && !strings.ContainsAny(message, "\n\r\x00") {
			return errors.New(message)
		}
		return errors.New("电脑运行空间准备失败，固定目录、权限或挂载不符合镜像约定")
	}
	return nil
}
