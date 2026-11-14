package deployment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestServerInstallDoesNotResetUnstartedUnit(t *testing.T) {
	installer, host := serverInstallFixture(t)
	installServerFixture(t, installer)
	if slices.Contains(host.calls, "reset-failed tomd.service") {
		t.Fatal("首次安装不应重置尚未启动的单元")
	}
	if !host.active || !host.enabled {
		t.Fatal("未启动服务或设置开机启动")
	}
}

func TestServerStartRecoversFailedUnit(t *testing.T) {
	installer, host := serverInstallFixture(t)
	host.failed = true
	if err := installer.startServer(t.Context()); err != nil {
		t.Fatal(err)
	}
	reset := slices.Index(host.calls, "reset-failed tomd.service")
	start := slices.Index(host.calls, "enable --now tomd.service")
	if reset < 0 || reset >= start || host.failed || !host.active {
		t.Fatal("未在启动前清除失败限流", host.calls)
	}
}

func TestServerStartStopsOnSystemctlFailure(t *testing.T) {
	for _, failing := range []string{"daemon-reload", "show", "reset-failed", "enable"} {
		t.Run(failing, func(t *testing.T) {
			installer, host := serverInstallFixture(t)
			host.failed = true
			run := installer.run
			failure := errors.New("模拟服务管理失败")
			failed := false
			installer.run = func(ctx context.Context, command serverCommand) ([]byte, error) {
				if failed {
					t.Fatal("命令失败后仍继续执行", command.arguments)
				}
				if command.arguments[0] == failing {
					failed = true
					return nil, failure
				}
				return run(ctx, command)
			}
			err := installer.startServer(t.Context())
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), "systemctl "+failing) {
				t.Fatal("命令失败缺少操作或底层原因", err)
			}
			if failing != "daemon-reload" && !strings.Contains(err.Error(), "tomd.service") {
				t.Fatal("命令失败缺少服务名称", err)
			}
		})
	}
}

func TestServerStartRejectsUnavailableUnit(t *testing.T) {
	for _, properties := range []string{"", "LoadState=not-found\nActiveState=inactive\n", "LoadState=masked\nActiveState=inactive\n", "LoadState=loaded\n"} {
		t.Run(properties, func(t *testing.T) {
			installer, host := serverInstallFixture(t)
			run := installer.run
			installer.run = func(ctx context.Context, command serverCommand) ([]byte, error) {
				if command.arguments[0] == "show" {
					return []byte(properties), nil
				}
				return run(ctx, command)
			}
			if err := installer.startServer(t.Context()); err == nil || host.active {
				t.Fatal("单元状态异常仍启动服务", err)
			}
		})
	}
}

func TestServerUpgradeRecoversInstallationBeforeFirstStart(t *testing.T) {
	installer, host := serverInstallFixture(t)
	run := installer.run
	installer.run = func(ctx context.Context, command serverCommand) ([]byte, error) {
		if command.arguments[0] == "daemon-reload" {
			return nil, errors.New("模拟安装发布后启动前中断")
		}
		return run(ctx, command)
	}
	if _, err := installer.manage(t.Context(), "install", ServerInstallOptions{URL: "https://192.0.2.20:9443"}, "/source/tomd"); err == nil {
		t.Fatal("安装中断没有报告失败")
	}
	installer.run = run
	key := readServerFixture(t, installer, serverDirectory+"/identity.key")
	candidate := readServerFixture(t, installer, serverBinaryPath)
	candidate[len(candidate)-1] = 4
	writeInstallFixture(t, installer.fs, "/source/new-tomd", candidate, 0755)
	report, err := installer.manage(t.Context(), "upgrade", ServerInstallOptions{}, "/source/new-tomd")
	if err != nil || !report.Healthy || host.backups != 1 {
		t.Fatal("修复版不能接续首次启动前中断的安装", report, err)
	}
	if !slices.Equal(key, readServerFixture(t, installer, serverDirectory+"/identity.key")) {
		t.Fatal("升级重新生成了安装身份")
	}
}
