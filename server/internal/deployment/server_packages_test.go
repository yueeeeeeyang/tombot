package deployment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresPackagesFollowDistributionAndKeepExistingCluster(t *testing.T) {
	for _, id := range []string{"ubuntu", "debian", "rhel", "rocky", "almalinux", "centos", "fedora"} {
		for _, existing := range []bool{false, true} {
			t.Run(id+map[bool]string{false: "首次", true: "重试"}[existing], func(t *testing.T) {
				installer, _ := serverInstallFixture(t)
				apt := id == "ubuntu" || id == "debian"
				writeInstallFixture(t, installer.fs, "/etc/os-release", []byte("ID="+id+"\nVERSION_ID=26\nVERSION_CODENAME=example\n"), 0644)
				if existing && !apt {
					writeInstallFixture(t, installer.fs, "/var/lib/pgsql/data/PG_VERSION", []byte("17\n"), 0600)
				}
				var calls []string
				installer.run = func(_ context.Context, command serverCommand) ([]byte, error) {
					calls = append(calls, command.program+" "+strings.Join(command.arguments, " "))
					if command.program == "/usr/bin/dpkg-query" || command.program == "/usr/bin/rpm" {
						if !existing {
							return nil, &serverCommandExit{program: filepath.Base(command.program), code: 1}
						}
						if apt {
							return []byte("installed\n"), nil
						}
						return []byte(command.arguments[len(command.arguments)-1] + "\n"), nil
					}
					if command.program == "/usr/bin/postgresql-setup" && strings.Join(command.environment, " ") != "PGSETUP_INITDB_OPTIONS=--auth-local=peer --auth-host=scram-sha-256" {
						t.Fatal("初始化未使用密码认证")
					}
					return nil, nil
				}
				if err := installer.ensurePostgresPackages(t.Context()); err != nil {
					t.Fatal(err)
				}
				all := strings.Join(calls, "\n")
				manager := "/usr/bin/dnf"
				if apt {
					manager = "/usr/bin/apt-get"
				}
				if strings.Contains(all, manager) == existing {
					t.Fatal("包查询没有控制安装行为", all)
				}
				if strings.Contains(all, "postgresql-setup") != (!apt && !existing) {
					t.Fatal("已有集群被重新初始化或新 RPM 集群未初始化", all)
				}
				if !strings.Contains(all, "systemctl enable --now postgresql.service") {
					t.Fatal("未启动数据库")
				}
			})
		}
	}
}

func TestPostgresPackageFailuresStopBeforeStart(t *testing.T) {
	for _, kind := range []string{"查询失败", "状态损坏", "安装失败", "集群残留", "初始化失败"} {
		t.Run(kind, func(t *testing.T) {
			installer, _ := serverInstallFixture(t)
			writeInstallFixture(t, installer.fs, "/etc/os-release", []byte("ID=rocky\nVERSION_ID=9.7\n"), 0644)
			if kind == "集群残留" {
				writeInstallFixture(t, installer.fs, "/var/lib/pgsql/data/postgresql.conf", []byte("已有配置"), 0600)
			}
			installer.run = func(_ context.Context, command serverCommand) ([]byte, error) {
				switch command.program {
				case "/usr/bin/rpm":
					if kind == "查询失败" {
						return nil, &serverCommandExit{program: "rpm", code: 2}
					}
					if kind == "状态损坏" {
						return []byte("重复或异常记录"), nil
					}
					return nil, &serverCommandExit{program: "rpm", code: 1}
				case "/usr/bin/dnf":
					if kind == "安装失败" {
						return nil, errors.New("安装失败")
					}
				case "/usr/bin/postgresql-setup":
					return nil, errors.New("初始化失败")
				case "/usr/bin/systemctl":
					t.Fatal("失败后仍启动数据库")
				}
				return nil, nil
			}
			if err := installer.ensurePostgresPackages(t.Context()); err == nil {
				t.Fatal("错误状态仍成功")
			}
			if kind == "集群残留" {
				if _, err := os.Stat(installer.fs.resolve("/var/lib/pgsql/data/postgresql.conf")); err != nil {
					t.Fatal("残留数据被清理")
				}
			}
		})
	}
}
