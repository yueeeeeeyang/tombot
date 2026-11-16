package dockerbootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/linuxhost"
)

func TestUbuntuBasePackagesAndUpdatedRevisionsAreReused(t *testing.T) {
	for _, versions := range [][3]string{{"1.47.0-2.4~exp1ubuntu4", "2.39.3-9ubuntu6", "2.39.3-9ubuntu6"}, {"1.47.0-2.4~exp1ubuntu4.8", "2.39.3-9ubuntu6.9", "2.39.3-9ubuntu6.9"}} {
		fixture := newFixture(t, true)
		for index, name := range []string{"e2fsprogs", "util-linux", "mount"} {
			fixture.packages[name] = commandResult{output: []byte(name + "\tamd64\t" + versions[index] + "\tinstall ok installed\n")}
		}
		report, err := fixture.installation.run(t.Context())
		if err != nil {
			t.Fatal("基础系统或安全更新被拒绝", err)
		}
		for index, actual := range report.HostPackages {
			if actual.Version != versions[index] {
				t.Fatal("报告没有记录真实版本")
			}
		}
		if strings.Contains(strings.Join(fixture.commands, "\n"), "apt-get") {
			t.Fatal("已有可用系统包被重装")
		}
		delete(fixture.packages, "mount")
		fixture.commands = nil
		if _, err := fixture.installation.run(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, command := range fixture.commands {
			if strings.Contains(command, "apt-get") && strings.Contains(command, " install ") && (!strings.HasSuffix(command, " mount") || strings.Contains(command, "e2fsprogs")) {
				t.Fatal("补装缺包影响了既有软件", command)
			}
		}
	}
}

func TestRPMBootstrapInstallsFromMatchingSignedRepository(t *testing.T) {
	for _, id := range []string{"rhel", "rocky", "almalinux", "centos", "fedora"} {
		t.Run(id, func(t *testing.T) {
			fixture := newFixture(t, false)
			encoded := []byte("ID=" + id + "\nVERSION_ID=9\n")
			fixture.file(t, "etc/os-release", encoded, 0644)
			release, err := linuxhost.ParseRelease(encoded)
			if err != nil {
				t.Fatal(err)
			}
			fixture.installation.release = release
			fixture.fingerprint = release.DockerKeyFingerprint()
			for _, name := range []string{"dnf-3", "rpm"} {
				fixture.file(t, "usr/bin/"+name, []byte("测试程序"), 0755)
			}
			if err := os.Symlink("dnf-3", fixture.installation.host.path("usr/bin/dnf")); err != nil {
				t.Fatal(err)
			}
			runner := fixture.installation.host.run
			fixture.installation.host.run = func(ctx context.Context, program string, args ...string) (commandResult, error) {
				switch filepath.Base(program) {
				case "rpm":
					name := args[len(args)-1]
					key := strings.TrimSuffix(name, ".x86_64")
					if item, present := fixture.packages[key]; present {
						return item, nil
					}
					return commandResult{exitCode: 1, output: []byte("package " + name + " is not installed\n")}, nil
				case "dnf":
					return runner(ctx, fixture.installation.host.path("usr/bin/apt-get"), args...)
				default:
					return runner(ctx, program, args...)
				}
			}
			report, err := fixture.installation.run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(report.HostPackages) != 2 {
				t.Fatal("RPM 平台使用了 Debian 包名")
			}
			source, err := os.ReadFile(fixture.installation.host.path("etc/yum.repos.d/tombot-docker.repo"))
			if err != nil || string(source) != release.DockerSource("amd64") {
				t.Fatal("RPM 软件源错误", err)
			}
			if _, err := os.Stat(fixture.installation.host.path("etc/apt/sources.list.d/tombot-docker.sources")); !os.IsNotExist(err) {
				t.Fatal("RPM 引导写入了 APT 软件源")
			}
		})
	}
}

func TestPackageManagerLinkRejectsUnsafeTargets(t *testing.T) {
	for _, name := range []string{"正常入口", "目标可写", "目标不可执行", "跨目录", "链接链", "目标缺失", "父目录可写"} {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t, true)
			fixture.installation.release = linuxhost.Release{Family: "dnf"}
			fixture.file(t, "usr/bin/dnf-3", []byte("测试程序"), 0755)
			target := "dnf-3"
			switch name {
			case "目标可写":
				if err := os.Chmod(fixture.installation.host.path("usr/bin/dnf-3"), 0777); err != nil {
					t.Fatal(err)
				}
			case "目标不可执行":
				if err := os.Chmod(fixture.installation.host.path("usr/bin/dnf-3"), 0644); err != nil {
					t.Fatal(err)
				}
			case "跨目录":
				target = "../bin/dnf-3"
			case "链接链":
				if err := os.Symlink("dnf-3", fixture.installation.host.path("usr/bin/alternate")); err != nil {
					t.Fatal(err)
				}
				target = "alternate"
			case "目标缺失":
				target = "missing"
			case "父目录可写":
				if err := os.Chmod(fixture.installation.host.path("usr/bin"), 0777); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, fixture.installation.host.path("usr/bin/dnf")); err != nil {
				t.Fatal(err)
			}
			exists, err := fixture.installation.packageManagerExecutable()
			if (exists && err == nil) != (name == "正常入口") {
				t.Fatal("包管理器入口核验结果不符", exists, err)
			}
		})
	}
}

func TestDockerAPIRequirements(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"APIVersion":"1.47","MinAPIVersion":"1.24"}`, true}, {`{"APIVersion":"1.53","MinAPIVersion":"1.44"}`, true},
		{`{"APIVersion":"1.46","MinAPIVersion":"1.24"}`, false}, {`{"APIVersion":"1.53","MinAPIVersion":"1.48"}`, false}, {`{}`, false}, {`{"APIVersion":"latest"}`, false},
	} {
		if (verifyDockerAPI([]byte(test.body)) == nil) != test.valid {
			t.Fatal("API 能力边界错误", test.body)
		}
	}
}

func TestDistributionDockerVersionsUseCapabilities(t *testing.T) {
	for _, version := range []string{"27.5.1+dfsg1", "28.0.0", "29.7.2", "30.1.0-ce"} {
		fixture := newFixture(t, true)
		fixture.engineVersion = version
		report, err := fixture.installation.run(t.Context())
		if err != nil || report.Version != version {
			t.Fatal("合规发行版版本仍被白名单拒绝", version, err)
		}
	}
}
