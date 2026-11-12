package dockerbootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type commandResult struct {
	output   []byte
	exitCode int
}
type commandRunner func(context.Context, string, ...string) (commandResult, error)

type host struct {
	root            string
	owner           uint32
	isRoot          bool
	operatingSystem string
	architecture    string
	run             commandRunner
	fetch           func(context.Context) ([]byte, error)
	freeSpace       func(string) (uint64, error)
}

func realHost() host {
	return host{root: "/", owner: 0, isRoot: os.Geteuid() == 0, operatingSystem: runtime.GOOS, architecture: runtime.GOARCH, run: runCommand, fetch: fetchRepositoryKey, freeSpace: diskFree}
}

func (host host) path(relative string) string { return filepath.Join(host.root, relative) }

// readProtected 不跟随文件末级符号链接，并拒绝非预期所有者或可被组及其他用户改写的配置。
func (host host) readProtected(path string, limit int64) (contents []byte, result error) {
	if err := host.checkParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != host.owner || !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 || stat.Size() > limit {
		return nil, errors.New("Docker 引导文件所有者、权限或类型无效")
	}
	contents, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("Docker 引导文件过大")
	}
	return contents, nil
}

func (host host) directory(relative string, mode os.FileMode) (string, error) {
	path := host.path(relative)
	relative, err := filepath.Rel(host.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Docker 引导目录超出受控根目录")
	}
	current := host.root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err = os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err = host.checkDirectory(current); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (host host) checkDirectory(path string) error {
	stat, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != host.owner || !stat.IsDir() || stat.Mode().Perm()&0022 != 0 {
		return errors.New("Docker 引导目录权限或所有者无效")
	}
	return nil
}

func (host host) checkParents(path string) error {
	relative, err := filepath.Rel(host.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Docker 引导文件超出受控根目录")
	}
	current := host.root
	if err = host.checkDirectory(current); err != nil {
		return err
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err = host.checkDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func diskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func writeAtomic(directory, name string, contents []byte, mode os.FileMode) (result error) {
	file, err := os.CreateTemp(directory, ".tombot-write-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return errors.Join(err, file.Close())
	}
	_, writeErr := file.Write(contents)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err = os.Rename(path, filepath.Join(directory, name)); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func (host host) lock(ctx context.Context) (release func() error, result error) {
	directory, err := host.directory(strings.TrimPrefix(RuntimeDirectory, "/"), 0700)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "tombot-docker-bootstrap.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != host.owner || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(errors.New("Docker 引导锁权限无效"), file.Close())
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error { return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(err, file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-ticker.C:
		}
	}
}

type limitedOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (output *limitedOutput) Write(value []byte) (int, error) {
	const maxOutput = 1 << 20
	remaining := maxOutput - output.buffer.Len()
	if len(value) > remaining {
		output.overflow = true
	}
	if remaining > 0 {
		if _, err := output.buffer.Write(value[:min(len(value), remaining)]); err != nil {
			return 0, err
		}
	}
	return len(value), nil
}

func runCommand(ctx context.Context, program string, arguments ...string) (commandResult, error) {
	command := exec.CommandContext(ctx, program, arguments...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "DEBIAN_FRONTEND=noninteractive"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 3 * time.Second
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	var output limitedOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	err := command.Run()
	if ctx.Err() != nil {
		return commandResult{}, ctx.Err()
	}
	if output.overflow {
		return commandResult{}, errors.New("Docker 引导命令输出超过限制")
	}
	result := commandResult{output: output.buffer.Bytes()}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.exitCode = exited.ExitCode()
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("无法执行引导程序 %s", filepath.Base(program))
	}
	return result, nil
}

func fetchRepositoryKey(ctx context.Context) (contents []byte, result error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Docker 签名下载不允许重定向") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://download.docker.com/linux/ubuntu/gpg", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("下载 Docker 仓库签名失败")
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Docker 仓库签名下载状态无效")
	}
	contents, err = io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(contents) > 64*1024 || len(contents) == 0 {
		return nil, errors.New("Docker 仓库签名内容无效")
	}
	return contents, nil
}
