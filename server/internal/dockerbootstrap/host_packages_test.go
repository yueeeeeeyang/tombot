package dockerbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExistingDockerInstallsMissingHostPackages(t *testing.T) {
	fixture := newFixture(t, true)
	delete(fixture.packages, "e2fsprogs")
	if err := os.Remove(fixture.installation.host.path("usr/sbin/e2fsck")); err != nil {
		t.Fatal(err)
	}
	report, err := fixture.installation.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.HostPackages, fixture.installation.hostPackages()) || !reflect.DeepEqual(fixture.state(t).Report.HostPackages, report.HostPackages) {
		t.Fatal("未保存实际宿主软件包验证结果")
	}
	commands := strings.Join(fixture.commands, "\n")
	for _, needed := range []string{"update", "--no-remove", "--no-upgrade", "--no-install-recommends", "e2fsprogs:amd64=1.47.0-test1", "util-linux:amd64=2.39.3-test1", "mount:amd64=2.39.3-test1", "e2fsck -V", "fallocate --version", "losetup --version"} {
		if !strings.Contains(commands, needed) {
			t.Fatal("已有 Docker 路径未完成固定依赖安装和可执行检查", needed)
		}
	}
	if strings.Contains(commands, "docker-ce=") || fixture.downloads != 0 {
		t.Fatal("补齐磁盘工具不应重新安装已有 Docker 或重写其软件源")
	}
	fixture.commands = nil
	if _, err = fixture.installation.run(context.Background()); err != nil || strings.Contains(strings.Join(fixture.commands, "\n"), "apt-get") {
		t.Fatal("重复启动未按真实包状态跳过安装", err)
	}
}

func TestHostPackageConflictsBeforeMutations(t *testing.T) {
	valid := "e2fsprogs\tamd64\t1.47.0-test1\tinstall ok installed\n"
	cases := map[string]commandResult{
		"版本不符":  {output: []byte(strings.Replace(valid, "1.47.0-test1", "1.47.0-other", 1))},
		"架构不符":  {output: []byte(strings.Replace(valid, "amd64", "arm64", 1))},
		"包名不符":  {output: []byte(strings.Replace(valid, "e2fsprogs", "other", 1))},
		"未配置":   {output: []byte(strings.Replace(valid, "ok installed", "ok unpacked", 1))},
		"半配置":   {output: []byte(strings.Replace(valid, "ok installed", "ok half-configured", 1))},
		"损坏":    {output: []byte(strings.Replace(valid, "ok installed", "reinstreq installed", 1))},
		"待删除":   {output: []byte(strings.Replace(valid, "install ok", "deinstall ok", 1))},
		"持有但缺失": {output: []byte(strings.Replace(valid, "install ok installed", "hold ok not-installed", 1))},
		"重复记录":  {output: []byte(valid + valid)},
		"空记录":   {},
		"查询失败":  {exitCode: 2},
		"部分失败":  {output: []byte(valid), exitCode: 1},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, true)
			fixture.packages["e2fsprogs"] = result
			delete(fixture.packages, "mount")
			if _, err := fixture.installation.run(context.Background()); err == nil {
				t.Fatal("异常包状态被接受")
			}
			commands := strings.Join(fixture.commands, "\n")
			if strings.Contains(commands, "apt-get") || strings.Contains(commands, "systemctl") || fixture.downloads != 0 {
				t.Fatal("未知宿主状态仍执行了系统修改")
			}
			if persisted := fixture.state(t); persisted.Status != "failed" || persisted.Report != nil {
				t.Fatal("失败仍留下成功报告")
			}
		})
	}
}

func TestHostToolsVerifiedOnEveryStart(t *testing.T) {
	for _, scenario := range []string{"工具缺失", "无执行位", "工具可写", "父目录可写", "链接越界", "链接失效", "程序失败"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t, true)
			if _, err := fixture.installation.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := fixture.installation.host.path("usr/sbin/e2fsck")
			var err error
			switch scenario {
			case "工具缺失":
				err = os.Remove(path)
			case "无执行位":
				err = os.Chmod(path, 0644)
			case "工具可写":
				err = os.Chmod(path, 0777)
			case "父目录可写":
				err = os.Chmod(filepath.Dir(path), 0777)
			case "链接越界", "链接失效":
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := fixture.installation.host.path("usr/sbin/missing")
				if scenario == "链接越界" {
					target = filepath.Join(t.TempDir(), "other")
					if err = os.WriteFile(target, []byte("测试外部工具"), 0755); err != nil {
						t.Fatal(err)
					}
				}
				err = os.Symlink(target, path)
			case "程序失败":
				fixture.failure = "e2fsck"
			}
			if err != nil {
				t.Fatal(err)
			}
			fixture.commands = nil
			if _, err = fixture.installation.run(context.Background()); err == nil {
				t.Fatal("历史成功状态掩盖了当前工具故障")
			}
			if persisted := fixture.state(t); persisted.Status != "failed" || persisted.Report != nil {
				t.Fatal("没有撤销上次引导成功证据")
			}
			if strings.Contains(strings.Join(fixture.commands, "\n"), "systemctl") {
				t.Fatal("工具失效后仍启动 Docker 服务")
			}
		})
	}
}

func TestHostPackageInstallRecheckedAndRecovered(t *testing.T) {
	for _, scenario := range []string{"未落盘", "版本漂移", "安装结果未知"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t, true)
			delete(fixture.packages, "e2fsprogs")
			runner := fixture.installation.host.run
			fixture.installation.host.run = func(ctx context.Context, program string, arguments ...string) (commandResult, error) {
				result, err := runner(ctx, program, arguments...)
				if filepath.Base(program) == "apt-get" && strings.Contains(strings.Join(arguments, " "), "e2fsprogs:") {
					switch scenario {
					case "未落盘":
						delete(fixture.packages, "e2fsprogs")
					case "版本漂移":
						fixture.packages["e2fsprogs"] = commandResult{output: []byte("e2fsprogs\tamd64\t1.47.0-other\tinstall ok installed\n")}
					case "安装结果未知":
						return commandResult{}, context.DeadlineExceeded
					}
				}
				return result, err
			}
			if _, err := fixture.installation.run(context.Background()); err == nil {
				t.Fatal("安装命令返回成功或结果未知被当作已验证")
			}
			if fixture.state(t).Status != "failed" {
				t.Fatal("未保留安装失败状态")
			}
			if scenario == "安装结果未知" {
				fixture.installation.host.run = runner
				fixture.commands = nil
				if _, err := fixture.installation.run(context.Background()); err != nil {
					t.Fatal("重启没有根据实际已安装状态恢复", err)
				}
				if strings.Contains(strings.Join(fixture.commands, "\n"), "apt-get") {
					t.Fatal("重启盲目重放了结果未知的安装")
				}
			}
		})
	}
}

func TestHostPackageRemovedAndHeldStates(t *testing.T) {
	for _, status := range []string{"deinstall ok config-files", "purge ok not-installed", "hold ok installed"} {
		t.Run(status, func(t *testing.T) {
			fixture := newFixture(t, true)
			fixture.packages["e2fsprogs"] = commandResult{output: []byte("e2fsprogs\tamd64\t1.47.0-test1\t" + status + "\n")}
			if _, err := fixture.installation.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			installed := strings.Contains(strings.Join(fixture.commands, "\n"), "e2fsprogs:amd64=")
			if installed == strings.HasPrefix(status, "hold") {
				t.Fatal("未区分移除残留与已持有安装包")
			}
		})
	}
}

func TestHostToolTrustedSymlinkAndCancellation(t *testing.T) {
	fixture := newFixture(t, true)
	path := fixture.installation.host.path("usr/sbin/mkfs.ext4")
	if err := os.Rename(path, fixture.installation.host.path("usr/sbin/mke2fs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mke2fs", path); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.installation.run(context.Background()); err != nil {
		t.Fatal("拒绝了发行版受保护工具链接", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := fixture.installation.host.run
	fixture.installation.host.run = func(ctx context.Context, program string, arguments ...string) (commandResult, error) {
		if filepath.Base(program) == "mke2fs" {
			cancel()
			return commandResult{}, ctx.Err()
		}
		return runner(ctx, program, arguments...)
	}
	if _, err := fixture.installation.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("工具检查吞掉取消", err)
	}
	if fixture.state(t).Status != "failed" {
		t.Fatal("取消后仍报告成功")
	}
}
