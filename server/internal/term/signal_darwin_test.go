package term

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinExitedGroupPermissionRace(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := command.Wait(); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		members, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", command.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) == 1 && members[0].Proc.P_stat == processZombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("子进程未进入待回收状态")
		}
		time.Sleep(time.Millisecond)
	}
	if err := signalGroup(command.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal("已退出进程组被误报权限失败", err)
	}
	if err := normalizeGroupSignalError(syscall.Getpgrp(), syscall.EPERM); !errors.Is(err, syscall.EPERM) {
		t.Fatal("活进程组权限错误被隐藏", err)
	}
}
