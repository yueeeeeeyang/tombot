package dockernode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"io.yueyang/tom/internal/storagehost"
)

type storageResult struct {
	output   []byte
	exitCode int
}
type storageCommand func(context.Context, string, ...string) (storageResult, error)

type storageOutput struct{ buffer bytes.Buffer }

func (output *storageOutput) Write(contents []byte) (int, error) {
	if output.buffer.Len()+len(contents) > 2<<20 {
		return 0, errors.New("持久磁盘命令输出超过限制")
	}
	return output.buffer.Write(contents)
}

func storageCommands(timeout time.Duration) (storageCommand, error) {
	programs := make(map[string]string)
	for _, program := range storagehost.Programs() {
		resolved, err := filepath.EvalSymlinks(program.Path)
		if err != nil {
			return nil, errors.New("持久磁盘所需宿主工具缺失")
		}
		if err = checkProtectedParents(filepath.Dir(resolved)); err != nil {
			return nil, err
		}
		stat, err := os.Lstat(resolved)
		if err != nil {
			return nil, errors.New("读取持久磁盘工具属性失败")
		}
		owner, ok := stat.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 || stat.Mode().Perm()&0111 == 0 {
			return nil, errors.New("持久磁盘宿主工具权限无效")
		}
		programs[program.Name] = resolved
	}
	return func(ctx context.Context, name string, args ...string) (storageResult, error) {
		program, ok := programs[name]
		if !ok {
			return storageResult{}, errors.New("持久磁盘命令不在允许范围")
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		command := exec.CommandContext(ctx, program, args...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error {
			err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		command.WaitDelay = 3 * time.Second
		var output storageOutput
		command.Stdout = &output
		command.Stderr = io.Discard
		err := command.Run()
		if ctx.Err() != nil {
			return storageResult{}, ctx.Err()
		}
		if err == nil {
			return storageResult{output: output.buffer.Bytes()}, nil
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return storageResult{output: output.buffer.Bytes(), exitCode: exited.ExitCode()}, nil
		}
		return storageResult{}, errors.New("执行持久磁盘宿主工具失败")
	}, nil
}
