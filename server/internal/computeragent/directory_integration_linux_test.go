//go:build linux && agentintegration && !cgo

package computeragent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/taskprocess"
)

func TestKernelDesktopDirectoryLifecycle(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" {
		t.Fatal("桌面目录测试必须运行在专用受限容器")
	}
	if err := taskprocess.CheckController(); err != nil {
		t.Fatal(err)
	}
	if err := prepareFilesystem(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t)
	first := fixture.command.Session
	secondCommand, _ := desktopCommand(fixture.command, 2)
	second := secondCommand.Session
	directories := make([]*desktopDirectory, 0, 2)
	for _, session := range []computer.ExecutionSession{first, second} {
		directory, err := newDesktopDirectory(computer.DesktopRuntimeRoot, session)
		if err != nil {
			t.Fatal(err)
		}
		directories = append(directories, directory)
		t.Cleanup(func() {
			if err := directory.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, name := range []string{"", "config", "cache", "state", "data", "tmp", "passwd", "group"} {
			metadata, err := os.Lstat(filepath.Join(session.RuntimeDirectory(), name))
			if err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0700)
			if name == "passwd" || name == "group" {
				mode = 0600
			}
			identity := metadata.Sys().(*syscall.Stat_t)
			if metadata.Mode().Perm() != mode || identity.Uid != uint32(session.UID) || identity.Gid != uint32(session.GID) {
				t.Fatal("图形身份目录权限或所属身份错误", name)
			}
		}
	}
	if duplicate, err := newDesktopDirectory(computer.DesktopRuntimeRoot, first); err == nil || duplicate != nil {
		t.Fatal("重复授权覆盖了已有桌面目录")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `test -r "$1/passwd" && test ! -r "$2/passwd" && test ! -w /run/bot-desktops`, "_", first.RuntimeDirectory(), second.RuntimeDirectory())
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(first.UID), Gid: uint32(first.GID)}}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("真实 Bot 身份未保持独立桌面目录：%v，%s", err, output)
	}
	marker := "/workspace/desktop-cleanup-marker"
	if err := os.WriteFile(marker, []byte("shared-preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/workspace", filepath.Join(first.RuntimeDirectory(), "outside")); err != nil {
		t.Fatal(err)
	}
	scope := &kernelScope{directory: directories[0], membershipUsed: true}
	if err := scope.Close(); err == nil {
		t.Fatal("入组已发放但未排空时清理了桌面目录")
	}
	if _, err := os.Stat(filepath.Join(first.RuntimeDirectory(), "passwd")); err != nil {
		t.Fatal("排空确认前身份文件被删除", err)
	}
	scope.drained = true
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.RuntimeDirectory()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧会话图形目录未清理", err)
	}
	if _, err := os.Stat(filepath.Join(second.RuntimeDirectory(), "passwd")); err != nil {
		t.Fatal("关闭一桌面清理了另一个桌面", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "shared-preserved" {
		t.Fatal("桌面清理跟随链接修改了共享业务目录", err)
	}
}

func TestKernelDesktopDirectoryRejectsUnsafeRoots(t *testing.T) {
	if os.Getenv("TOM_AGENT_ENTRY_TEST") == "" || os.Geteuid() != 0 {
		t.Fatal("目录权限测试必须运行在专用受限 root 容器")
	}
	fixture := newFixture(t)
	root := t.TempDir()
	for _, mode := range []os.FileMode{0700, 0755, 0777} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if directory, err := newDesktopDirectory(root, fixture.command.Session); err == nil || directory != nil {
			t.Fatal("接受了错误桌面根目录权限", mode)
		}
	}
	if err := os.Chmod(root, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 0, 20000); err != nil {
		t.Fatal(err)
	}
	if _, err := newDesktopDirectory(root, fixture.command.Session); err == nil {
		t.Fatal("接受了公共组所有的控制根目录")
	}
	if err := os.Chown(root, 0, 0); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newDesktopDirectory(link, fixture.command.Session); err == nil {
		t.Fatal("接受了链接控制根目录")
	}
}
