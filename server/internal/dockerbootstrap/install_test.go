package dockerbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/storagehost"
)

type fixture struct {
	installation  installer
	commands      []string
	downloads     int
	failure       string
	fingerprint   string
	conflict      bool
	engineVersion string
	missingQuota  bool
	packages      map[string]commandResult
}

func testManifest() Manifest {
	return Manifest{EnginePackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", CLIPackageVersion: "5:29.7.2-1~ubuntu.24.04~noble", ContainerdPackageVersion: "2.3.6-1~ubuntu.24.04~noble",
		E2fsprogsPackageVersion: "1.47.0-test1", UtilLinuxPackageVersion: "2.39.3-test1", MountPackageVersion: "2.39.3-test1",
		AllowedEngineVersions: []string{"29.7.2"}, RepositoryFingerprint: strings.Repeat("A", 40), TimeoutSeconds: 300, MinimumFreeBytes: 1 << 30}
}

func newFixture(t *testing.T, installed bool) *fixture {
	t.Helper()
	fixture := &fixture{fingerprint: strings.Repeat("A", 40), engineVersion: "29.7.2", packages: make(map[string]commandResult)}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := host{root: root, owner: uint32(os.Getuid()), isRoot: true, operatingSystem: "linux", architecture: "amd64", freeSpace: func(string) (uint64, error) { return 10 << 30, nil }}
	fixture.installation = installer{host: host, manifest: testManifest(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), started: time.Now().UTC()}
	for _, directory := range []string{"etc", "usr/bin", "usr/sbin", "run/systemd/system", "sys/fs/cgroup"} {
		if _, err := host.directory(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	fixture.file(t, "etc/os-release", []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n"), 0644)
	fixture.file(t, "sys/fs/cgroup/cgroup.controllers", []byte("cpuset cpu io memory pids\n"), 0644)
	for _, program := range []string{"apt-get", "dpkg-query", "systemctl"} {
		fixture.file(t, "usr/bin/"+program, []byte("测试程序占位"), 0755)
	}
	if installed {
		fixture.file(t, "usr/bin/docker", []byte("测试程序占位"), 0755)
		fixture.installHostPackages(t)
	}
	fixture.installation.host.fetch = func(context.Context) ([]byte, error) {
		fixture.downloads++
		return []byte("测试公开签名材料"), nil
	}
	fixture.installation.host.run = func(_ context.Context, program string, arguments ...string) (commandResult, error) {
		name := filepath.Base(program)
		fixture.commands = append(fixture.commands, name+" "+strings.Join(arguments, " "))
		if fixture.failure == name {
			return commandResult{exitCode: 1}, nil
		}
		switch name {
		case "dpkg-query":
			if len(arguments) == 3 && strings.Contains(arguments[2], ":") {
				name, _, _ := strings.Cut(arguments[2], ":")
				if result, exists := fixture.packages[name]; exists {
					return result, nil
				}
				return commandResult{exitCode: 1}, nil
			}
			if fixture.conflict {
				return commandResult{output: []byte("containerd\tinstalled\n"), exitCode: 1}, nil
			}
			return commandResult{exitCode: 1}, nil
		case "apt-get":
			for _, argument := range arguments {
				if strings.HasPrefix(argument, "e2fsprogs:") {
					fixture.installHostPackages(t)
				}
				if strings.HasPrefix(argument, "docker-ce=") {
					fixture.file(t, "usr/bin/docker", []byte("测试程序占位"), 0755)
				}
			}
			return commandResult{}, nil
		case "gpg":
			return commandResult{output: []byte("pub:-:4096:1:public::::::\nfpr:::::::::" + fixture.fingerprint + ":\nsub:-:4096:1:public::::::\nfpr:::::::::" + strings.Repeat("B", 40) + ":\n")}, nil
		case "systemctl":
			return commandResult{}, nil
		case "fallocate", "mkfs.ext4", "mke2fs", "e2fsck", "losetup", "mount", "umount":
			if len(arguments) != 1 || arguments[0] != "--version" && arguments[0] != "-V" {
				t.Fatal("宿主工具检查执行了非版本查询", name)
			}
			return commandResult{}, nil
		case "docker":
			if len(arguments) == 1 && arguments[0] == "--version" {
				return commandResult{output: []byte("Docker version " + fixture.engineVersion + ", build test\n")}, nil
			}
			encoded, err := json.Marshal(map[string]any{"ServerVersion": fixture.engineVersion, "OSType": "linux", "CgroupVersion": "2", "MemoryLimit": true, "PidsLimit": true, "CPUCfsQuota": !fixture.missingQuota, "SecurityOptions": []string{"name=seccomp,profile=builtin"}})
			return commandResult{output: encoded}, err
		default:
			t.Fatal("引导调用了未声明的程序", name)
			return commandResult{}, errors.New("未知测试程序")
		}
	}
	return fixture
}

func (fixture *fixture) installHostPackages(t *testing.T) {
	t.Helper()
	for _, expected := range fixture.installation.hostPackages() {
		fixture.packages[expected.Name] = commandResult{output: []byte(expected.Name + "\t" + expected.Architecture + "\t" + expected.Version + "\tinstall ok installed\n")}
	}
	for _, program := range storagehost.Programs() {
		fixture.file(t, strings.TrimPrefix(program.Path, "/"), []byte("测试程序占位"), 0755)
	}
}

func (fixture *fixture) file(t *testing.T, relative string, contents []byte, mode os.FileMode) {
	t.Helper()
	path := fixture.installation.host.path(relative)
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func (fixture *fixture) state(t *testing.T) state {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(fixture.installation.stateDirectory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state state
	if err = json.Unmarshal(encoded, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestFreshInstallAndRepeatedBootstrap(t *testing.T) {
	fixture := newFixture(t, false)
	report, err := fixture.installation.run(context.Background())
	if err != nil || report.Version != "29.7.2" {
		t.Fatal("新安装流程失败", err)
	}
	if fixture.state(t).Status != "succeeded" || fixture.state(t).Stage != "docker_ready" {
		t.Fatal("未记录真实验证结果")
	}
	commands := strings.Join(fixture.commands, "\n")
	for _, required := range []string{"DPkg::Lock::Timeout=60", "Acquire::Retries=2", "--no-remove", "docker-ce=" + fixture.installation.manifest.EnginePackageVersion, "docker-ce-cli=" + fixture.installation.manifest.CLIPackageVersion, "containerd.io=" + fixture.installation.manifest.ContainerdPackageVersion, "systemctl enable --now docker.service"} {
		if !strings.Contains(commands, required) {
			t.Fatal("安装动作缺少固定版本、锁或服务步骤", required)
		}
	}
	for _, forbidden := range []string{"curl ", " sh ", "sudo ", "apt-get remove", "usermod"} {
		if strings.Contains(commands, forbidden) {
			t.Fatal("引导使用了不允许的安装方式")
		}
	}
	source, err := os.ReadFile(fixture.installation.host.path("etc/apt/sources.list.d/tombot-docker.sources"))
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != repositorySource("amd64") {
		t.Fatal("Docker 来源不受固定签名配置约束")
	}
	fixture.commands = nil
	if _, err = fixture.installation.run(context.Background()); err != nil {
		t.Fatal("重复引导失败", err)
	}
	if strings.Contains(strings.Join(fixture.commands, "\n"), "apt-get") || fixture.downloads != 1 {
		t.Fatal("已有兼容 Docker 被重复安装或重新下载")
	}
}

func TestBootstrapFailuresDoNotDeclareReady(t *testing.T) {
	for _, scenario := range []string{"签名不符", "冲突软件", "软件源失败", "启动失败", "版本不符", "资源限制缺失", "发行版不符", "磁盘不足", "cgroup 缺失"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newFixture(t, false)
			switch scenario {
			case "签名不符":
				fixture.fingerprint = strings.Repeat("B", 40)
			case "冲突软件":
				fixture.conflict = true
			case "软件源失败":
				fixture.failure = "apt-get"
			case "启动失败":
				fixture.failure = "systemctl"
			case "版本不符":
				fixture.engineVersion = "28.0.0"
			case "资源限制缺失":
				fixture.missingQuota = true
			case "发行版不符":
				fixture.file(t, "etc/os-release", []byte("ID=debian\nVERSION_ID=13\n"), 0644)
			case "磁盘不足":
				fixture.installation.host.freeSpace = func(string) (uint64, error) { return 1, nil }
			case "cgroup 缺失":
				fixture.file(t, "sys/fs/cgroup/cgroup.controllers", []byte("cpu memory"), 0644)
			}
			if _, err := fixture.installation.run(context.Background()); err == nil {
				t.Fatal("引导失败场景被当成就绪")
			}
			if state := fixture.state(t); state.Status != "failed" || state.Report != nil {
				t.Fatal("失败状态被丢弃或仍包含就绪报告")
			}
			if scenario == "签名不符" || scenario == "冲突软件" {
				for _, command := range fixture.commands {
					if strings.Contains(command, "docker-ce=") {
						t.Fatal("签名或冲突检查失败仍安装 Docker")
					}
				}
			}
		})
	}
	t.Run("已有不兼容运行时不修改", func(t *testing.T) {
		fixture := newFixture(t, true)
		fixture.engineVersion = "28.0.0"
		if _, err := fixture.installation.run(context.Background()); err == nil {
			t.Fatal("已有未知版本被接受")
		}
		if len(fixture.commands) != 1 || !strings.Contains(fixture.commands[0], "docker --version") {
			t.Fatal("未知运行时触发安装或服务变更")
		}
	})
}

func TestProtectedFilesDirectoriesAndLock(t *testing.T) {
	fixture := newFixture(t, false)
	host := fixture.installation.host
	path := host.path("etc/manifest.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.readProtected(path, 1024); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := host.readProtected(path, 1024); err == nil {
		t.Fatal("可被其他用户改写的清单被接受")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host.path("etc/os-release"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := host.readProtected(path, 1024); err == nil {
		t.Fatal("清单允许末级符号链接替换")
	}
	release, err := host.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if _, err = host.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("重复引导未被互斥锁阻止", err)
	}
	cancel()
	if err = release(); err != nil {
		t.Fatal(err)
	}
	release, err = host.lock(context.Background())
	if err != nil {
		t.Fatal("退出后未释放引导锁", err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	if _, err = host.directory("var/lib", 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, host.path("var/lib/tombot-node")); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.installation.run(context.Background()); err == nil {
		t.Fatal("引导沿父目录链接写出受控目录")
	}
	if _, err = os.Stat(filepath.Join(outside, "bootstrap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("拒绝链接前已经写入其他目录")
	}
}

func TestManifestAndFingerprintValidation(t *testing.T) {
	encoded, err := json.Marshal(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseManifest(encoded); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*Manifest){
		func(value *Manifest) { value.EnginePackageVersion = "latest" },
		func(value *Manifest) { value.CLIPackageVersion = "1; arbitrary-command" },
		func(value *Manifest) { value.E2fsprogsPackageVersion = "" },
		func(value *Manifest) { value.UtilLinuxPackageVersion = "latest" },
		func(value *Manifest) { value.MountPackageVersion = "2 *" },
		func(value *Manifest) { value.AllowedEngineVersions = []string{"29"} },
		func(value *Manifest) { value.RepositoryFingerprint = "" },
		func(value *Manifest) { value.TimeoutSeconds = 0 },
		func(value *Manifest) { value.MinimumFreeBytes = 0 },
	} {
		value := testManifest()
		mutation(&value)
		encoded, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ParseManifest(encoded); err == nil {
			t.Fatal("安装清单允许无界或不准确参数")
		}
	}
	for _, contents := range []string{"", "pub:::::::::\nsub:::::::::\n", "fpr:::::::::" + strings.Repeat("A", 40) + ":\n", "pub:::::::::\nfpr:::::::::" + strings.Repeat("B", 40) + ":\n", "pub:::::::::\nfpr:::::::::" + strings.Repeat("A", 40) + ":\npub:::::::::\n"} {
		if err := verifyFingerprint([]byte(contents), strings.Repeat("A", 40)); err == nil {
			t.Fatal("缺失、错误或多个主签名被接受")
		}
	}
}

func TestCommandCancellationAndOutputBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := runCommand(ctx, "/bin/sh", "-c", "sleep 10"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("引导命令没有按时取消", err)
	}
	if _, err := runCommand(context.Background(), "/usr/bin/head", "-c", "1048577", "/dev/zero"); err == nil {
		t.Fatal("引导命令输出没有上限")
	}
}
