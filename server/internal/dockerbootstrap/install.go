package dockerbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"io.yueyang/tom/internal/linuxhost"
)

// Report 仅证明 Docker 基础能力和宿主磁盘工具已核验；真实挂载、配额、镜像及进程探针另由节点执行。
type Report struct {
	Version         string          `json:"version"`
	OperatingSystem string          `json:"operating_system"`
	CgroupVersion   string          `json:"cgroup_version"`
	HostPackages    []PackageReport `json:"host_packages"`
}

type installer struct {
	release        linuxhost.Release
	host           host
	manifest       Manifest
	logger         *slog.Logger
	stateDirectory string
	stage          string
	started        time.Time
}
type state struct {
	Stage     string    `json:"stage"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ErrorCode string    `json:"error_code,omitempty"`
	Report    *Report   `json:"report,omitempty"`
}

// Install 只从受保护文件读取安装清单，在本机互斥锁内按真实状态安装、启动和复验 Docker。
// 非 Linux、非 root、未知发行版或既有运行时冲突均明确失败，不卸载其他工作负载。
func Install(ctx context.Context, manifestPath string, logger *slog.Logger) (report Report, result error) {
	host := realHost()
	if host.operatingSystem != "linux" || !host.isRoot {
		return report, errors.New("Docker 自动引导必须由 Linux 节点的受限 root 启动单元执行")
	}
	if logger == nil || !filepath.IsAbs(manifestPath) {
		return report, errors.New("Docker 自动引导缺少绝对清单路径或日志配置")
	}
	encoded, err := host.readProtected(manifestPath, 64*1024)
	if err != nil {
		return report, errors.New("读取受保护的 Docker 安装清单失败")
	}
	manifest, err := ParseManifest(encoded)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(manifest.TimeoutSeconds)*time.Second)
	defer cancel()
	release, err := host.lock(ctx)
	if err != nil {
		return report, err
	}
	defer func() { result = errors.Join(result, release()) }()
	installation := installer{host: host, manifest: manifest, logger: logger, started: time.Now().UTC()}
	return installation.run(ctx)
}

func (installation *installer) persist(status, code string, report *Report) error {
	encoded, err := json.Marshal(state{Stage: installation.stage, Status: status, StartedAt: installation.started, UpdatedAt: time.Now().UTC(), ErrorCode: code, Report: report})
	if err != nil {
		return err
	}
	if err = writeAtomic(installation.stateDirectory, "state.json", encoded, 0600); err != nil {
		return errors.New("写入 Docker 引导进度失败")
	}
	installation.logger.Info("Docker 引导阶段已记录", "stage", installation.stage, "status", status)
	return nil
}

func (installation *installer) step(stage string) error {
	installation.stage = stage
	return installation.persist("running", "", nil)
}

func (installation *installer) run(ctx context.Context) (report Report, result error) {
	if installation.host.operatingSystem != "linux" || !installation.host.isRoot || installation.host.architecture != "amd64" && installation.host.architecture != "arm64" {
		return report, errors.New("Docker 引导要求 Linux root 环境及 amd64 或 arm64 架构")
	}
	directory, err := installation.host.directory(strings.TrimPrefix(StateDirectory, "/"), 0700)
	if err != nil {
		return report, err
	}
	installation.stateDirectory = directory
	defer func() {
		if result != nil {
			result = errors.Join(result, installation.persist("failed", "bootstrap_step_failed", nil))
		}
	}()
	if err = installation.step("validating_host"); err != nil {
		return report, err
	}
	if err = installation.preflight(); err != nil {
		return report, err
	}
	installed, err := installation.executable("usr/bin/docker")
	if err != nil {
		return report, err
	}
	if installed {
		output, err := installation.command(ctx, "usr/bin/docker", "--version")
		if err != nil {
			return report, err
		}
		match := regexp.MustCompile(`^Docker version ([^,\n]{1,100}),`).FindStringSubmatch(string(output))
		if len(match) != 2 || !supportsDockerCLI(match[1]) {
			return report, errors.New("已有 Docker 不支持节点所需 API，请升级 Docker；未自动覆盖或卸载")
		}
	}
	missing, err := installation.missingHostPackages(ctx)
	if err != nil {
		return report, err
	}
	if !installed {
		if err = installation.conflicts(ctx); err != nil {
			return report, err
		}
	}
	if missing {
		if err = installation.installHostPackages(ctx); err != nil {
			return report, err
		}
	}
	if !installed {
		if err = installation.repository(ctx); err != nil {
			return report, err
		}
		if err = installation.step("installing_engine"); err != nil {
			return report, err
		}
		if err = installation.installPackages(ctx, "docker-ce", "docker-ce-cli", "containerd.io"); err != nil {
			return report, err
		}
	}
	if err = installation.step("verifying_host_dependencies"); err != nil {
		return report, err
	}
	packages, err := installation.verifyHostPackages(ctx)
	if err != nil {
		return report, err
	}
	if err = installation.step("starting_engine"); err != nil {
		return report, err
	}
	if _, err = installation.command(ctx, "usr/bin/systemctl", "enable", "--now", "docker.service"); err != nil {
		return report, err
	}
	if err = installation.step("verifying_engine"); err != nil {
		return report, err
	}
	output, err := installation.command(ctx, "usr/bin/docker", "--host", "unix://"+SocketPath, "info", "--format", "{{json .}}")
	if err != nil {
		return report, err
	}
	report, err = verifyEngine(output)
	if err != nil {
		return report, err
	}
	api, err := installation.command(ctx, "usr/bin/docker", "--host", "unix://"+SocketPath, "version", "--format", "{{json .Server}}")
	if err != nil {
		return report, err
	}
	if err := verifyDockerAPI(api); err != nil {
		return report, err
	}
	report.HostPackages = packages
	installation.stage = "docker_ready"
	if err = installation.persist("succeeded", "", &report); err != nil {
		return Report{}, err
	}
	return report, nil
}

func (installation *installer) executable(relative string) (bool, error) {
	path := installation.host.path(relative)
	if err := installation.host.checkParents(filepath.Dir(path)); err != nil {
		return false, err
	}
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != installation.host.owner || !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 || stat.Mode().Perm()&0111 == 0 {
		return false, errors.New("Docker 引导工具权限或所有者无效")
	}
	return true, nil
}

// packageManagerExecutable 允许发行版自带的同目录入口链接，仍核验链接所有者和实际程序。
// DNF 常以 dnf -> dnf-3 或 dnf5 发布；不接受跨目录、链接链或可写目标。
func (installation *installer) packageManagerExecutable() (bool, error) {
	relative := strings.TrimPrefix(installation.release.Manager(), "/")
	path := installation.host.path(relative)
	if err := installation.host.checkParents(filepath.Dir(path)); err != nil {
		return false, err
	}
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stat.Mode()&os.ModeSymlink == 0 {
		return installation.executable(relative)
	}
	owner, ok := stat.Sys().(*syscall.Stat_t)
	target, err := os.Readlink(path)
	if err != nil || !ok || owner.Uid != installation.host.owner || target == "." || target == ".." || filepath.Base(target) != target {
		return false, errors.New("系统包管理器链接所有者或目标无效")
	}
	return installation.executable(filepath.Join(filepath.Dir(relative), target))
}

func (installation *installer) preflight() error {
	releasePath := installation.host.path("etc/os-release")
	stat, err := os.Lstat(releasePath)
	if err != nil {
		return errors.New("缺少 Linux 发行信息")
	}
	if stat.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(releasePath)
		if err != nil || resolved != installation.host.path("usr/lib/os-release") {
			return errors.New("Linux 发行信息指向未知位置")
		}
		releasePath = resolved
	}
	encoded, err := installation.host.readProtected(releasePath, 64*1024)
	if err != nil {
		return err
	}
	if installation.release, err = linuxhost.ParseRelease(encoded); err != nil {
		return err
	}
	manager, err := os.Stat(installation.host.path("run/systemd/system"))
	if err != nil || !manager.IsDir() {
		return errors.New("Docker 自动引导要求 systemd 系统服务环境")
	}
	query := "usr/bin/dpkg-query"
	if installation.release.Family == "dnf" {
		query = "usr/bin/rpm"
	}
	for _, program := range []string{strings.TrimPrefix(installation.release.Manager(), "/"), query, "usr/bin/systemctl"} {
		var exists bool
		var err error
		if program == strings.TrimPrefix(installation.release.Manager(), "/") {
			exists, err = installation.packageManagerExecutable()
		} else {
			exists, err = installation.executable(program)
		}
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("Docker 自动引导缺少系统包管理或服务工具")
		}
	}
	controllers, err := installation.host.readProtected(installation.host.path("sys/fs/cgroup/cgroup.controllers"), 4096)
	if err != nil {
		return errors.New("Docker 电脑节点必须启用 cgroup v2")
	}
	available := strings.Fields(string(controllers))
	for _, needed := range []string{"cpu", "memory", "pids"} {
		found := false
		for _, controller := range available {
			if needed == controller {
				found = true
			}
		}
		if !found {
			return errors.New("Docker 电脑节点缺少必要 cgroup 控制器")
		}
	}
	free, err := installation.host.freeSpace(installation.stateDirectory)
	if err != nil {
		return errors.New("读取 Docker 节点磁盘余量失败")
	}
	if free < installation.manifest.MinimumFreeBytes {
		return errors.New("Docker 节点磁盘余量不足")
	}
	return nil
}

func (installation *installer) command(ctx context.Context, program string, arguments ...string) ([]byte, error) {
	result, err := installation.host.run(ctx, installation.host.path(program), arguments...)
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, errors.New("Docker 引导命令执行失败：" + filepath.Base(program))
	}
	return result.output, nil
}

func (installation *installer) apt(ctx context.Context, arguments ...string) ([]byte, error) {
	options := []string{"-o", "DPkg::Lock::Timeout=60", "-o", "Acquire::Retries=2", "-o", "APT::Update::Error-Mode=any", "-o", "Acquire::http::Timeout=30", "-o", "Acquire::https::Timeout=30"}
	return installation.command(ctx, "usr/bin/apt-get", append(options, arguments...)...)
}

func (installation *installer) conflicts(ctx context.Context) error {
	if installation.release.Family == "dnf" {
		for _, name := range []string{"docker", "docker-client", "docker-common", "docker-engine", "podman-docker", "podman", "runc"} {
			result, err := installation.host.run(ctx, installation.host.path("usr/bin/rpm"), "-q", "--qf", "%{NAME}\n", name)
			if err != nil {
				return err
			}
			if result.exitCode == 0 {
				return errors.New("发现冲突容器软件包：" + name + "；未自动卸载")
			}
			if result.exitCode != 1 || strings.TrimSpace(string(result.output)) != "package "+name+" is not installed" {
				return errors.New("读取容器软件包状态失败")
			}
		}
		return nil
	}
	result, err := installation.host.run(ctx, installation.host.path("usr/bin/dpkg-query"), "--show", "--showformat=${binary:Package}\t${db:Status-Status}\n", "docker.io", "docker-compose", "docker-compose-v2", "docker-doc", "docker-buildx", "podman-docker", "containerd", "runc")
	if err != nil {
		return err
	}
	if result.exitCode > 1 {
		return errors.New("读取现有容器软件包状态失败")
	}
	for _, line := range strings.Split(string(result.output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "installed" {
			return errors.New("发现冲突的容器软件包，不自动卸载或覆盖现有工作负载")
		}
	}
	return nil
}

func verifyEngine(encoded []byte) (Report, error) {
	var engine struct {
		ServerVersion   string
		OSType          string
		CgroupVersion   string
		MemoryLimit     bool
		PidsLimit       bool
		CPUCfsQuota     bool
		SecurityOptions []string
	}
	if err := json.Unmarshal(encoded, &engine); err != nil {
		return Report{}, errors.New("Docker 服务状态响应无效")
	}
	if !supportsDockerCLI(engine.ServerVersion) || engine.OSType != "linux" || engine.CgroupVersion != "2" || !engine.MemoryLimit || !engine.PidsLimit || !engine.CPUCfsQuota {
		return Report{}, errors.New("Docker 版本、cgroup 或资源限制能力不符合要求")
	}
	seccomp := false
	for _, option := range engine.SecurityOptions {
		if strings.HasPrefix(option, "name=seccomp,") && !strings.Contains(option, "unconfined") {
			seccomp = true
		}
	}
	if !seccomp {
		return Report{}, errors.New("Docker 默认系统调用过滤未启用")
	}
	return Report{Version: engine.ServerVersion, OperatingSystem: engine.OSType, CgroupVersion: engine.CgroupVersion}, nil
}

// installPackages 使用本发行版签名仓库；不使用跨发行版包、不移除冲突工作负载。
func (installation *installer) installPackages(ctx context.Context, packages ...string) error {
	if installation.release.Family == "apt" {
		if _, err := installation.apt(ctx, "update"); err != nil {
			return err
		}
		_, err := installation.apt(ctx, append([]string{"install", "--yes", "--no-remove", "--no-upgrade", "--no-install-recommends"}, packages...)...)
		return err
	}
	_, err := installation.command(ctx, "usr/bin/dnf", append([]string{"--assumeyes", "--setopt=timeout=30", "--setopt=retries=2", "--setopt=install_weak_deps=False", "install"}, packages...)...)
	return err
}

// supportsDockerCLI 只要求包含卷子目录 API 的引擎代际，实际服务的 API 范围另行核对。
func supportsDockerCLI(version string) bool {
	parts := regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.+~-]+)?$`).FindStringSubmatch(version)
	if parts == nil {
		return false
	}
	major, err := strconv.Atoi(parts[1])
	return err == nil && major >= 27
}

func verifyDockerAPI(encoded []byte) error {
	var version struct {
		APIVersion    string
		MinAPIVersion string
	}
	if json.Unmarshal(encoded, &version) != nil {
		return errors.New("Docker API 能力响应无效")
	}
	minor := func(value string) (int, error) {
		if !regexp.MustCompile(`^1\.[0-9]+$`).MatchString(value) {
			return 0, errors.New("API 版本无效")
		}
		return strconv.Atoi(strings.TrimPrefix(value, "1."))
	}
	maximum, maxErr := minor(version.APIVersion)
	minimum, minErr := minor(version.MinAPIVersion)
	if maxErr != nil || minErr != nil || minimum > 47 || maximum < 47 {
		return errors.New("Docker 服务不支持节点所需 Engine API 1.47")
	}
	return nil
}
