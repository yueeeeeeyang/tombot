package dockerbootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"io.yueyang/tom/internal/storagehost"
)

// PackageReport 记录宿主包管理器查询确认的软件包，不把安装命令成功作为完成证据。
type PackageReport struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

// hostPackages 按发行家族固定包名和宿主架构，外部清单不能扩大安装集合。
func (installation *installer) hostPackages() []PackageReport {
	names := []string{"e2fsprogs", "util-linux"}
	if installation.release.Family == "apt" {
		names = append(names, "mount")
	}
	packages := make([]PackageReport, 0, len(names))
	for _, name := range names {
		packages = append(packages, PackageReport{Name: name, Architecture: installation.release.PackageArchitecture(installation.host.architecture)})
	}
	return packages
}

// hostPackage 区分未安装、完整安装与损坏状态；不把查询失败或尚未配置的软件包当作可用。
func (installation *installer) hostPackage(ctx context.Context, expected PackageReport) (actual PackageReport, installed bool, err error) {
	program, arguments := "usr/bin/dpkg-query", []string{"--show", "--showformat=${Package}\t${Architecture}\t${Version}\t${Status}\n", expected.Name + ":" + expected.Architecture}
	if installation.release.Family == "dnf" {
		program, arguments = "usr/bin/rpm", []string{"-q", "--qf", "%{NAME}\t%{ARCH}\t%{VERSION}-%{RELEASE}\tinstall ok installed\n", expected.Name + "." + expected.Architecture}
	}
	result, err := installation.host.run(ctx, installation.host.path(program), arguments...)
	if err != nil {
		return actual, false, err
	}
	if result.exitCode == 1 && (len(result.output) == 0 || installation.release.Family == "dnf" && strings.TrimSpace(string(result.output)) == "package "+expected.Name+"."+expected.Architecture+" is not installed") {
		return actual, false, nil
	}
	if result.exitCode != 0 {
		return actual, false, errors.New("读取宿主磁盘软件包状态失败：" + expected.Name)
	}
	fields := strings.Split(strings.TrimSuffix(string(result.output), "\n"), "\t")
	if len(fields) != 4 || strings.Contains(fields[3], "\n") || fields[0] != expected.Name || fields[1] != expected.Architecture {
		return actual, false, errors.New("宿主磁盘软件包查询结果无效：" + expected.Name)
	}
	status := strings.Split(fields[3], " ")
	if len(status) != 3 || status[1] != "ok" {
		return actual, false, errors.New("宿主磁盘软件包状态异常：" + expected.Name)
	}
	if (status[0] == "install" || status[0] == "deinstall" || status[0] == "purge" || status[0] == "unknown") && (status[2] == "not-installed" || status[2] == "config-files") {
		return actual, false, nil
	}
	if (status[0] != "install" && status[0] != "hold") || status[2] != "installed" {
		return actual, false, errors.New("宿主磁盘软件包未完成配置或已安排移除：" + expected.Name)
	}
	if !regexp.MustCompile(`^[0-9][0-9A-Za-z.+:~_-]{0,100}$`).MatchString(fields[2]) {
		return actual, false, errors.New("宿主软件包版本记录无效：" + expected.Name)
	}
	return PackageReport{Name: fields[0], Architecture: fields[1], Version: fields[2]}, true, nil
}

// missingHostPackages 在修改宿主前检查全部包；即使已发现缺包，也不能跳过其他包的冲突。
func (installation *installer) missingHostPackages(ctx context.Context) (bool, error) {
	missing := false
	for _, expected := range installation.hostPackages() {
		_, installed, err := installation.hostPackage(ctx, expected)
		if err != nil {
			return false, err
		}
		missing = missing || !installed
	}
	return missing, nil
}

// installHostPackages 在整体引导时限内安装固定依赖，命令结果未知时立即失败，不自动重放。
func (installation *installer) installHostPackages(ctx context.Context) error {
	var missing []string
	for _, expected := range installation.hostPackages() {
		_, installed, err := installation.hostPackage(ctx, expected)
		if err != nil {
			return err
		}
		if !installed {
			missing = append(missing, expected.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := installation.step("installing_host_dependencies"); err != nil {
		return err
	}
	// 仅补齐缺失项，不重装已经通过包状态与程序检查的系统工具。
	return installation.installPackages(ctx, missing...)
}

// verifyHostPackages 每次根据真实包状态和程序启动返回证据，任何缺失、权限漂移或执行失败均阻断引导。
func (installation *installer) verifyHostPackages(ctx context.Context) ([]PackageReport, error) {
	var packages []PackageReport
	for _, expected := range installation.hostPackages() {
		actual, installed, err := installation.hostPackage(ctx, expected)
		if err != nil {
			return nil, err
		}
		if !installed {
			return nil, errors.New("宿主磁盘软件包安装后仍缺失：" + expected.Name)
		}
		packages = append(packages, actual)
	}
	for _, program := range storagehost.Programs() {
		path := installation.host.path(strings.TrimPrefix(program.Path, "/"))
		if err := installation.host.checkParents(filepath.Dir(path)); err != nil {
			return nil, err
		}
		// Ubuntu 的 mkfs.ext4 是受保护的 mke2fs 链接；既检查入口父目录，也检查解析后的目标。
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errors.New("宿主磁盘工具缺失或链接失效：" + program.Name)
		}
		relative, err := filepath.Rel(installation.host.root, resolved)
		if err != nil {
			return nil, errors.New("宿主磁盘工具路径无效：" + program.Name)
		}
		exists, err := installation.executable(relative)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errors.New("宿主磁盘工具不存在：" + program.Name)
		}
		if _, err = installation.command(ctx, relative, program.VersionArgument); err != nil {
			return nil, err
		}
	}
	return packages, nil
}
