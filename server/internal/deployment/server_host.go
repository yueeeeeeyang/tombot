package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"time"
)

type serverCommand struct {
	program     string
	arguments   []string
	input       []byte
	output      io.Writer
	environment []string
	timeout     time.Duration
}

type serverRunner func(context.Context, serverCommand) ([]byte, error)

type serverCommandExit struct {
	program string
	code    int
}

func (failure *serverCommandExit) Error() string {
	return fmt.Sprintf("服务部署命令执行失败：%s（退出码 %d）；原始输出已隐藏", failure.program, failure.code)
}

// runServerCommand 不经过 Shell，不继承数据库认证环境，不回传可能包含机密的原始错误输出。
// 子进程组随取消一并终止；捕获输出有上限，安装包下载及备份使用各自显式时限。
func runServerCommand(parent context.Context, request serverCommand) ([]byte, error) {
	if request.timeout <= 0 || request.timeout > time.Hour || !filepath.IsAbs(request.program) {
		return nil, errors.New("服务部署命令或超时无效")
	}
	ctx, cancel := context.WithTimeout(parent, request.timeout)
	defer cancel()
	command := exec.CommandContext(ctx, request.program, request.arguments...)
	command.Env = append([]string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "SYSTEMD_PAGER=cat"}, request.environment...)
	command.Stdin = bytes.NewReader(request.input)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	var captured boundedServerOutput
	command.Stdout = &captured
	if request.output != nil {
		command.Stdout = request.output
	}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("服务部署命令取消或超时，请检查实际状态后重试")
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return nil, &serverCommandExit{program: filepath.Base(request.program), code: exited.ExitCode()}
		}
		return nil, errors.New("无法执行服务部署命令：" + filepath.Base(request.program))
	}
	return captured.Bytes(), nil
}

type boundedServerOutput struct{ bytes.Buffer }

func (output *boundedServerOutput) Write(content []byte) (int, error) {
	if output.Len()+len(content) > 1<<20 {
		return 0, errors.New("部署命令输出超过限制")
	}
	return output.Buffer.Write(content)
}

func ensureServerAccount(ctx context.Context, create bool) (account, error) {
	_, err := user.Lookup("tombot-api")
	if err != nil {
		var missing user.UnknownUserError
		if !create || !errors.As(err, &missing) {
			return account{}, errors.New("无法读取服务账户")
		}
		if _, err := runServerCommand(ctx, serverCommand{program: "/usr/sbin/useradd", arguments: []string{"--system", "--user-group", "--home-dir", "/opt/tombot", "--no-create-home", "--shell", "/usr/sbin/nologin", "tombot-api"}, timeout: 30 * time.Second}); err != nil {
			return account{}, err
		}
	}
	return lookupAccount()
}
