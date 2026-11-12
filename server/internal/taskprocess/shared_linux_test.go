//go:build linux && taskintegration && !cgo

package taskprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// prepareDirectories 仅为受限容器测试准备空白目录；正式部署仍须由代理创建和管理运行目录。
func prepareDirectories(t *testing.T, request *Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request.WorkingDirectory = "/workspace/runs/" + request.Group.Session.RunID
	for _, name := range []string{"/workspace", "/workspace/runs", "/workspace/.tmp", "/workspace/tools", "/workspace/tools/bin", request.WorkingDirectory} {
		if err := os.MkdirAll(name, 0770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(name, 0, request.Group.Session.GID); err != nil {
			t.Fatal(err)
		}
		// 六项最小权限不包含 FSETID；以公共组执行目录授权，避免内核清除不属于当前组的 setgid 位。
		permission := exec.CommandContext(ctx, "/bin/chmod", "2770", name)
		permission.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 0, Gid: uint32(request.Group.Session.GID)}}
		if output, err := permission.CombinedOutput(); err != nil {
			t.Fatalf("准备共享目录失败：%v\n%s", err, output)
		}
		if stat, err := os.Stat(name); err != nil || stat.Mode()&(os.ModeSetgid|0777) != os.ModeSetgid|0770 {
			t.Fatal("共享目录没有保留 setgid 与公共组权限", name, err)
		}
	}
	prepareDesktopDirectory(t, computer.DesktopRuntimeRoot, *request)
}

func prepareDesktopDirectory(t *testing.T, root string, request Request) {
	t.Helper()
	if err := os.MkdirAll(root, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0711); err != nil {
		t.Fatal(err)
	}
	identity := request.Group.Session
	base := filepath.Join(root, identity.SessionID)
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(base, identity.UID, identity.GID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "cache", "state", "data", "tmp"} {
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, identity.UID, identity.GID); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range map[string]string{
		"passwd":       fmt.Sprintf("bot%d:x:%d:%d:Bot:/workspace:/bin/bash\n", identity.UID, identity.UID, identity.GID),
		"group":        fmt.Sprintf("bot:x:%d:\n", identity.GID),
		"private-note": "desktop-private-example",
	} {
		path := filepath.Join(base, name)
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, identity.UID, identity.GID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKernelDesktopDirectoryBoundaries(t *testing.T) {
	requireIsolationContainer(t)
	request := validRequest()
	root := t.TempDir()
	prepareDesktopDirectory(t, root, request)
	verify := func() error {
		file, err := os.Open(root)
		if err != nil {
			return err
		}
		return verifyDesktopDirectoryAt(request, file)
	}
	if err := verify(); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, request.Group.Session.SessionID)
	if err := os.Rename(base, base+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base+"-original", base); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil {
		t.Fatal("桌面控制目录接受了符号链接")
	}
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(base+"-original", base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(base, request.Group.Session.UID+1, request.Group.Session.GID); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil {
		t.Fatal("接受了另一桌面的私有目录")
	}
	if err := os.Chown(base, request.Group.Session.UID, request.Group.Session.GID+1); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil {
		t.Fatal("接受了错误公共组的运行目录")
	}
	if err := os.Chown(base, request.Group.Session.UID, request.Group.Session.GID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0750); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil {
		t.Fatal("桌面控制目录向公共组开放")
	}
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0777, 0770, 0700} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := verify(); err == nil {
			t.Fatal("可替换或无法穿越的桌面根目录被接受", mode)
		}
	}
}

func TestKernelSharedFilesToolsAndPrivateDesktop(t *testing.T) {
	requireIsolationContainer(t)
	first := validRequest()
	prepareDirectories(t, &first)
	tool := "shared-tool-" + first.Group.Session.RunID
	first.Arguments = []string{"-c", `set -eu
test "$(id -u)" = 100001
test "$(id -g)" = 20000
test ! -e /proc/self/fd/3
test ! -e /proc/self/fd/4
test ! -e /proc/self/fd/5
test -z "${PARENT_ONLY:-}"
test "$HOME" = /workspace
test "$TMPDIR" = /workspace/.tmp
test "$DISPLAY" = :1
test -w /workspace
test -w /workspace/runs
if chmod 777 /run/bot-desktops; then exit 1; fi
printf example > created
printf temporary > "$TMPDIR/` + first.OperationID + `"
test "$(stat -c %a created)" = 660
test "$(stat -c %g created)" = 20000
mkdir output
test "$(stat -c %a output)" = 2770
printf '#!/bin/sh
printf shared' > /workspace/tools/bin/` + tool + `
chmod 770 /workspace/tools/bin/` + tool + `
test "$(ulimit -c)" = 0
cat /proc/self/status
`}
	output, err := runChild(t, first, "drop")
	if err != nil {
		t.Fatalf("稳定身份降权执行失败：%v\n%s", err, output)
	}
	status, err := parseStatus(output)
	if err != nil || !status.unprivileged(uint32(first.Group.Session.UID), uint32(first.Group.Session.GID)) {
		t.Fatalf("用户进程残留权限：%v\n%s", err, output)
	}
	if content, err := os.ReadFile(filepath.Join(first.WorkingDirectory, "created")); err != nil || string(content) != "example" {
		t.Fatal("用户程序未实际执行", err)
	}
	second := validRequest()
	second.Group.Session.UserID, second.Group.Session.ComputerID = first.Group.Session.UserID, first.Group.Session.ComputerID
	second.Group.Session.UID, second.Group.Session.DisplayNumber = computer.DesktopUIDBase+2, 2
	prepareDirectories(t, &second)
	// 业务目录可直接指向另一 Bot 的产出，普通符号链接保留文件系统语义。
	link := "/workspace/shared-link-" + ids.New()
	if err := os.Symlink(first.WorkingDirectory, link); err != nil {
		t.Fatal(err)
	}
	second.WorkingDirectory = link
	second.Arguments = []string{"-c", `set -eu
test "$(id -u)" = 100002
test "$(id -g)" = 20000
test "$DISPLAY" = :2
test "$(cat created)" = example
printf shared >> created
test "$(cat "$TMPDIR/` + first.OperationID + `")" = temporary
test "$( ` + tool + ` )" = shared
test ! -x '` + first.Group.Session.RuntimeDirectory() + `'
test ! -r '` + first.Group.Session.RuntimeDirectory() + `/private-note'
`}
	if output, err := runChild(t, second, "drop"); err != nil {
		t.Fatalf("跨 Bot 业务共享或桌面隔离失败：%v\n%s", err, output)
	}
	if content, err := os.ReadFile(filepath.Join(first.WorkingDirectory, "created")); err != nil || string(content) != "exampleshared" {
		t.Fatal("其他 Bot 无法直接修改共享产出", err)
	}
	second.WorkingDirectory = first.Group.Session.RuntimeDirectory()
	second.Arguments = []string{"-c", "exit 0"}
	output, err = runChild(t, second, "drop")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(output), "进入任务工作目录失败") {
		t.Fatalf("降权后仍能进入其他桌面：%v\n%s", err, output)
	}
	first.Program, first.Arguments = "/definitely-missing-program", nil
	output, err = runChild(t, first, "drop")
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(output), "启动任务用户程序失败") {
		t.Fatalf("启动失败被掩盖：%v\n%s", err, output)
	}
}
