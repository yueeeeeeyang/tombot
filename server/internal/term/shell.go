package term

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
)

const MaxOutputBytes = 16 << 20
const TailBytes = 32 << 10
const DefaultShellTimeout = 120 * time.Second
const MaxShellTimeout = 10 * time.Minute

type EnvironmentSecret struct {
	Name     string `json:"name"`
	SecretID string `json:"secret_id"`
}
type ShellInput struct {
	Command        string              `json:"command"`
	Cwd            string              `json:"cwd"`
	TimeoutSeconds int                 `json:"timeout_seconds"`
	EnvSecrets     []EnvironmentSecret `json:"env_secrets"`
}
type ShellResult struct {
	HasBinaryOutput bool   `json:"has_binary_output,omitempty"`
	ExitCode        int    `json:"exit_code"`
	StdoutTail      string `json:"stdout_tail"`
	Truncated       bool   `json:"truncated"`
	OutputPath      string `json:"output_path,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	Output          []byte `json:"-"`
}

func (r ShellResult) FailureCode() string { return r.ErrorCode }

// Shell 负责单次进程组生命周期；会话型终端由独立管理器保留。
type Shell struct {
	terminationGrace time.Duration
	maximumOutput    int
}

func NewShell() *Shell {
	return &Shell{terminationGrace: 5 * time.Second, maximumOutput: MaxOutputBytes}
}

func validEnvName(name string) bool {
	if name == "" || len(name) > 120 {
		return false
	}
	for index, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	switch name {
	case "PATH", "HOME", "TMPDIR", "SHELL", "PWD", "OLDPWD", "ENV", "BASH_ENV", "IFS", "SHELLOPTS", "BASHOPTS", "CDPATH", "GLOBIGNORE", "LANG", "LC_ALL", "TERM":
		return false
	}
	return !strings.HasPrefix(name, "LD_") && !strings.HasPrefix(name, "DYLD_") && !strings.HasPrefix(name, "BASH_FUNC_")
}
func ValidateShell(input ShellInput) error {
	if strings.TrimSpace(input.Command) == "" || len(input.Command) > 4096 || strings.ContainsRune(input.Command, 0) || !utf8.ValidString(input.Command) || input.TimeoutSeconds < 0 || input.TimeoutSeconds > int(MaxShellTimeout/time.Second) || len(input.EnvSecrets) > 16 {
		return domain.NewError(400, "INVALID_TOOL_INPUT", "Provide a command up to 4096 bytes and a timeout between 1 and 600 seconds.")
	}
	names := map[string]bool{}
	for _, ref := range input.EnvSecrets {
		if !validEnvName(ref.Name) || ref.SecretID == "" || len(ref.SecretID) > 120 || names[ref.Name] {
			return domain.NewError(400, "INVALID_TOOL_INPUT", "Secret environment names must be unique, uppercase and must not override process controls.")
		}
		names[ref.Name] = true
	}
	return nil
}
func ShellTimeout(input ShellInput) time.Duration {
	if input.TimeoutSeconds == 0 {
		return DefaultShellTimeout
	}
	return time.Duration(input.TimeoutSeconds) * time.Second
}
func signalGroup(pid int, signal syscall.Signal) error {
	err := normalizeGroupSignalError(pid, syscall.Kill(-pid, signal))
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("向进程组 %d 发送信号 %d 失败: %w", pid, signal, err)
	}
	return err
}

// Run 不继承服务端凭据；取消先通知整个组，宽限期后强制终止，并等待输出管道回收。
func (s *Shell) Run(ctx context.Context, input ShellInput, cwd, home string, secrets map[string][]byte) (result ShellResult, err error) {
	result.ExitCode = -1
	if err = ValidateShell(input); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	shell, args := "", []string{"-lc", input.Command}
	if shell, err = exec.LookPath("bash"); err == nil {
		args = append([]string{"--noprofile", "--norc"}, args...)
	} else if errors.Is(err, exec.ErrNotFound) {
		shell, err = exec.LookPath("sh")
	}
	if err != nil {
		return result, domain.NewError(503, "SHELL_UNAVAILABLE", "No supported shell is installed.")
	}
	environment := []string{"HOME=" + home, "TMPDIR=" + filepath.Join(home, ".tom", "tmp")}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "TERM"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	for name, value := range secrets {
		if !validEnvName(name) || len(value) == 0 || strings.IndexByte(string(value), 0) >= 0 {
			return result, domain.NewError(400, "SECRET_ENV_INVALID", "The secret cannot be injected as an environment value.")
		}
		environment = append(environment, name+"="+string(value))
	}
	defer clear(environment)
	command := exec.Command(shell, args...)
	command.Dir = cwd
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = s.terminationGrace
	execution, cancel := context.WithTimeout(ctx, ShellTimeout(input))
	defer cancel()
	output := newCapture(secrets, s.maximumOutput, cancel)
	command.Stdout, command.Stderr = output, output
	if err = command.Start(); err != nil {
		return result, domain.NewError(500, "SHELL_START_FAILED", "The command could not be started. Check its working directory and executable permissions.")
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	var waitErr, terminateErr error
	forceKilled := false
	select {
	case waitErr = <-finished:
	case <-execution.Done():
		terminateErr = signalGroup(command.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(s.terminationGrace)
		select {
		case waitErr = <-finished:
		case <-timer.C:
			killErr := signalGroup(command.Process.Pid, syscall.SIGKILL)
			forceKilled = killErr == nil
			terminateErr = errors.Join(terminateErr, killErr)
			waitErr = <-finished
		}
		timer.Stop()
	}
	// 即使父进程已退出，也回收同组的后台子进程；持久作业必须显式使用终端会话。
	if !forceKilled {
		terminateErr = errors.Join(terminateErr, signalGroup(command.Process.Pid, syscall.SIGKILL))
	}
	var exceeded bool
	var outputErr error
	result.Output, exceeded, outputErr = output.finish()
	if outputErr != nil {
		return result, errors.Join(outputErr, terminateErr)
	}
	result.HasBinaryOutput = !utf8.Valid(result.Output) || strings.IndexByte(string(result.Output), 0) >= 0
	result.Truncated = len(result.Output) > TailBytes || result.HasBinaryOutput
	tail := result.Output[max(0, len(result.Output)-TailBytes):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	result.StdoutTail = strings.ReplaceAll(strings.ToValidUTF8(string(tail), "�"), "\x00", `\x00`)
	result.ExitCode = command.ProcessState.ExitCode()
	if terminateErr != nil {
		return result, terminateErr
	}
	if ctx.Err() != nil {
		result.ErrorCode = "TOOL_CANCELED"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.ErrorCode = "TOOL_TIMEOUT"
		}
		return result, ctx.Err()
	}
	if exceeded {
		result.ErrorCode = "TOOL_OUTPUT_LIMIT"
		return result, nil
	}
	if errors.Is(execution.Err(), context.DeadlineExceeded) {
		result.ErrorCode = "TOOL_TIMEOUT"
		return result, nil
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			result.ErrorCode = "SHELL_EXIT_FAILED"
		} else {
			return result, waitErr
		}
	}
	return result, nil
}
