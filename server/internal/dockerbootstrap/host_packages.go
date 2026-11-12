package dockerbootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"io.yueyang/tom/internal/storagehost"
)

// PackageReport 记录实际 dpkg 查询确认的软件包，不把清单值直接当作安装成功证据。
type PackageReport struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

// hostPackages 固定包名和宿主架构，清单只能指定版本，不能扩大安装集合。
func (installation *installer) hostPackages() []PackageReport {
	return []PackageReport{
		{Name: "e2fsprogs", Version: installation.manifest.E2fsprogsPackageVersion, Architecture: installation.host.architecture},
		{Name: "util-linux", Version: installation.manifest.UtilLinuxPackageVersion, Architecture: installation.host.architecture},
		{Name: "mount", Version: installation.manifest.MountPackageVersion, Architecture: installation.host.architecture},
	}
}

// hostPackage 区分未安装、完整安装与损坏状态；不把查询失败或尚未配置的软件包当作可用。
func (installation *installer) hostPackage(ctx context.Context, expected PackageReport) (actual PackageReport, installed bool, err error) {
	result, err := installation.host.run(ctx, installation.host.path("usr/bin/dpkg-query"), "--show", "--showformat=${Package}\t${Architecture}\t${Version}\t${Status}\n", expected.Name+":"+expected.Architecture)
	if err != nil {
		return actual, false, err
	}
	if result.exitCode == 1 && len(result.output) == 0 {
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
	if fields[2] != expected.Version {
		return actual, false, errors.New("已有宿主磁盘软件包版本不匹配，不自动升级或降级：" + expected.Name)
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
	if err := installation.step("installing_host_dependencies"); err != nil {
		return err
	}
	if _, err := installation.apt(ctx, "update"); err != nil {
		return err
	}
	// 同时约束三个直接依赖，APT 不得升级已安装的工具或移除其他工作负载。
	arguments := []string{"install", "--yes", "--no-remove", "--no-upgrade", "--no-install-recommends"}
	for _, expected := range installation.hostPackages() {
		arguments = append(arguments, expected.Name+":"+expected.Architecture+"="+expected.Version)
	}
	_, err := installation.apt(ctx, arguments...)
	return err
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
