package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"io.yueyang/tom/internal/config"
)

// Runner 允许测试替换外部进程；所有调用必须受上下文截止时间约束。
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	LookPath(string) (string, error)
}
type CommandRunner struct{}

func (CommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = 5 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("命令 %s 执行失败: %w", name, err)
	}
	return output, nil
}

type Environment struct {
	OS, Arch, PackageManager string
	IsRoot                   bool
	Runner                   Runner
}

// Detect 明确检测受支持环境；缺少包管理器时返回可操作的失败。
func Detect(runner Runner) (Environment, error) {
	env := Environment{OS: runtime.GOOS, Arch: runtime.GOARCH, IsRoot: os.Geteuid() == 0, Runner: runner}
	if env.OS == "darwin" {
		return env, nil
	}
	if env.OS != "linux" {
		return env, fmt.Errorf("服务端仅支持 Linux 与 macOS")
	}
	for _, name := range []string{"apt-get", "dnf", "yum", "apk", "zypper"} {
		if _, err := runner.LookPath(name); err == nil {
			env.PackageManager = name
			break
		}
	}
	if env.PackageManager == "" {
		return env, fmt.Errorf("未找到受支持的包管理器，请使用 apt/dnf/yum/apk/zypper 发行版")
	}
	return env, nil
}

// PackageCommand 使用参数数组，包名来自内置映射，不经过 shell 拼接。
func PackageCommand(manager string, fonts bool) (string, []string, error) {
	packages := map[string][]string{
		"apt-get": {"libnss3", "libatk1.0-0", "libatk-bridge2.0-0", "libcups2", "libdrm2", "libxkbcommon0", "libxcomposite1", "libxdamage1", "libxfixes3", "libxrandr2", "libgbm1", "libasound2", "libpango-1.0-0", "libcairo2", "ca-certificates"},
		"dnf":     {"nss", "atk", "at-spi2-atk", "cups-libs", "libdrm", "libxkbcommon", "libXcomposite", "libXdamage", "libXfixes", "libXrandr", "mesa-libgbm", "alsa-lib", "pango", "cairo", "ca-certificates"},
		"apk":     {"nss", "atk", "at-spi2-atk", "cups-libs", "libdrm", "libxkbcommon", "libxcomposite", "libxdamage", "libxfixes", "libxrandr", "mesa-gbm", "alsa-lib", "pango", "cairo", "ca-certificates"},
		"zypper":  {"mozilla-nss", "atk", "at-spi2-atk", "libcups2", "libdrm2", "libxkbcommon0", "libXcomposite1", "libXdamage1", "libXfixes3", "libXrandr2", "libgbm1", "libasound2", "pango", "cairo", "ca-certificates"},
	}
	packages["yum"] = packages["dnf"]
	names, ok := packages[manager]
	if !ok {
		return "", nil, fmt.Errorf("未知包管理器")
	}
	if fonts {
		names = []string{map[string]string{"apt-get": "fonts-noto-cjk", "dnf": "google-noto-sans-cjk-fonts", "yum": "google-noto-sans-cjk-fonts", "apk": "font-noto-cjk", "zypper": "noto-sans-cjk-fonts"}[manager], "fontconfig"}
	}
	args := []string{"install", "-y"}
	switch manager {
	case "apt-get":
		args = append(args, "--no-install-recommends")
	case "apk":
		args = []string{"add", "--no-cache"}
	case "zypper":
		args = []string{"--non-interactive", "install", "--no-recommends"}
	}
	return manager, append(args, names...), nil
}

func (env Environment) install(ctx context.Context, fonts bool) error {
	if env.OS == "darwin" {
		return fmt.Errorf("macOS 字体或运行库不可用，请修复系统字体与系统更新后重试")
	}
	name, args, err := PackageCommand(env.PackageManager, fonts)
	if err != nil {
		return err
	}
	if !env.IsRoot {
		if _, err = env.Runner.Run(ctx, "sudo", "-n", "true"); err != nil {
			return fmt.Errorf("BOOTSTRAP_NEED_ROOT：请以管理员运行 %s %s", name, strings.Join(args, " "))
		}
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}
	if env.PackageManager == "apt-get" {
		updateName := "apt-get"
		updateArgs := []string{"update"}
		if !env.IsRoot {
			updateName = "sudo"
			updateArgs = []string{"-n", "apt-get", "update"}
		}
		if _, err = env.Runner.Run(ctx, updateName, updateArgs...); err != nil {
			return fmt.Errorf("软件源刷新失败，请恢复网络或配置离线软件源后重新体检：%w", err)
		}
	}
	_, err = env.Runner.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("请检查网络或离线软件源后运行 %s %s 并重新体检：%w", name, strings.Join(args, " "), err)
	}
	return nil
}

// DiskFree 返回实际文件系统剩余字节，探测失败直接返回错误。
func DiskFree(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// Steps 组装真实引导能力；缺失依赖不能用成功默认值绕过。
func Steps(cfg config.Config, db *sql.DB, runner Runner) []Step {
	var env Environment
	detect := func(ctx context.Context) error {
		var err error
		env, err = Detect(runner)
		if err != nil {
			return err
		}
		if env.OS == "linux" {
			release, err := os.ReadFile("/etc/os-release")
			if err != nil {
				return err
			}
			if !strings.Contains(string(release), "ID=") {
				return fmt.Errorf("无法识别发行版")
			}
			output, err := runner.Run(ctx, "ldd", "--version")
			if err != nil {
				return err
			}
			if strings.Contains(string(output), "musl") {
				return fmt.Errorf("Chrome for Testing 需要 glibc；请使用 Debian/Ubuntu/Fedora 或 glibc 运行环境")
			}
		}
		return nil
	}
	dirs := func(context.Context) error {
		for _, path := range []string{cfg.DataDir, cfg.WorkspaceDir, filepath.Join(cfg.DataDir, "runtime"), filepath.Join(cfg.WorkspaceDir, "shared"), filepath.Join(cfg.WorkspaceDir, "bots"), filepath.Join(cfg.WorkspaceDir, ".tom", "tmp")} {
			if err := os.MkdirAll(path, 0700); err != nil {
				return err
			}
		}
		return nil
	}
	checkDirs := func(context.Context) error {
		for _, path := range []string{cfg.DataDir, cfg.WorkspaceDir, filepath.Join(cfg.DataDir, "runtime"), filepath.Join(cfg.WorkspaceDir, "shared"), filepath.Join(cfg.WorkspaceDir, "bots"), filepath.Join(cfg.WorkspaceDir, ".tom", "tmp")} {
			file, err := os.CreateTemp(path, ".writable-*")
			if err != nil {
				return err
			}
			if err = file.Close(); err != nil {
				return err
			}
			if err = os.Remove(file.Name()); err != nil {
				return err
			}
			free, err := DiskFree(path)
			if err != nil {
				return err
			}
			if free < 2<<30 {
				return fmt.Errorf("磁盘可用空间不足 2GB")
			}
		}
		return nil
	}
	fonts := func(ctx context.Context) error {
		if env.OS == "darwin" {
			output, err := runner.Run(ctx, "system_profiler", "SPFontsDataType", "-json")
			if err != nil {
				return err
			}
			if !strings.Contains(string(output), "PingFang") {
				return fmt.Errorf("系统字体清单缺少苹方中文字体")
			}
			return nil
		}
		output, err := runner.Run(ctx, "fc-list", ":lang=zh", "family")
		if err != nil {
			return err
		}
		for _, family := range strings.Split(string(output), "\n") {
			if supportsChineseFont(family) {
				return nil
			}
		}
		return fmt.Errorf("未发现受支持的中文字体，请安装 Noto CJK 字体包")
	}
	libs := func(ctx context.Context) error {
		if env.OS == "darwin" {
			return nil
		}
		if _, err := os.Stat(BrowserPath(cfg.DataDir)); err != nil {
			return err
		}
		output, err := runner.Run(ctx, "ldd", BrowserPath(cfg.DataDir))
		if err != nil {
			return err
		}
		if strings.Contains(string(output), "not found") {
			return fmt.Errorf("浏览器运行库缺失")
		}
		return nil
	}
	return []Step{
		{"detect_os", "检测操作系统", "detecting", true, nil, detect},
		{"prepare_dirs", "准备数据与工作目录", "preparing", true, dirs, checkDirs},
		{"install_fonts", "安装中文字体", "preparing", true, func(ctx context.Context) error { return env.install(ctx, true) }, fonts},
		{"ensure_browser", "准备托管浏览器", "preparing", true, func(ctx context.Context) error { return EnsureBrowser(ctx, cfg.DataDir, cfg.BrowserArchive) }, func(context.Context) error { return VerifyBrowser(cfg.DataDir) }},
		{"install_runtime_libs", "安装浏览器运行库", "preparing", true, func(ctx context.Context) error { return env.install(ctx, false) }, libs},
		{"selfcheck_tls", "校验根证书", "selfcheck", true, nil, func(ctx context.Context) error { return checkSystemRoots(ctx, env) }},
		{"selfcheck_fts", "验证全文检索", "selfcheck", true, nil, func(ctx context.Context) error {
			_, err := db.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.tom_fts_check USING fts5(content); INSERT INTO temp.tom_fts_check VALUES('山巅测试');`)
			if err != nil {
				return err
			}
			var count int
			queryErr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM temp.tom_fts_check WHERE tom_fts_check MATCH '山巅测试'`).Scan(&count)
			_, dropErr := db.ExecContext(ctx, "DROP TABLE temp.tom_fts_check")
			if queryErr != nil || dropErr != nil {
				return errors.Join(queryErr, dropErr)
			}
			if count != 1 {
				return fmt.Errorf("全文检索自检失败")
			}
			return nil
		}},
		{"selfcheck_pty", "验证终端", "selfcheck", true, nil, checkPTY},
		{"selfcheck_browser", "验证浏览器与中文渲染", "selfcheck", true, nil, func(ctx context.Context) error { return CheckBrowser(ctx, BrowserPath(cfg.DataDir)) }},
		{"selfcheck_clock", "核对系统时间", "selfcheck", false, nil, func(ctx context.Context) error {
			address := cfg.ClockURL
			if address == "" {
				release, err := currentRelease()
				if err != nil {
					return err
				}
				address = release.archiveURL()
			}
			return checkClock(ctx, address, nil, time.Now)
		}},
		{"optional_tools", "检测可选工具", "selfcheck", false, nil, func(context.Context) error {
			missing := []string{}
			for _, name := range []string{"git", "curl", "unzip", "python3"} {
				if _, err := runner.LookPath(name); err != nil {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				return fmt.Errorf("可选工具未安装：%s", strings.Join(missing, "、"))
			}
			return nil
		}},
	}
}

func checkPTY(ctx context.Context) error {
	command := exec.CommandContext(ctx, "sh", "-c", "printf tom-pty-ok")
	terminal, err := pty.Start(command)
	if err != nil {
		return err
	}
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			terminal.Close()
		case <-finished:
		}
	}()
	output, readErr := io.ReadAll(terminal)
	close(finished)
	closeErr := terminal.Close()
	waitErr := command.Wait()
	// Linux PTY 在从端退出后用 EIO 表示流结束。
	if readErr != nil && !errors.Is(readErr, syscall.EIO) {
		return errors.Join(readErr, waitErr)
	}
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return closeErr
	}
	if waitErr != nil {
		return waitErr
	}
	if !strings.Contains(string(output), "tom-pty-ok") {
		return fmt.Errorf("终端自检输出不完整")
	}
	return nil
}
