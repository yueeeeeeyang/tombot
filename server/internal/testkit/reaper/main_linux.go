package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"syscall"
)

// main 仅用于无 init 回收器的验收容器，维持进程测试的完整回收断言。
func main() {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "缺少测试命令")
		os.Exit(2)
	}
	command := exec.Command(os.Args[1], os.Args[2:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if pid == command.Process.Pid {
			if err = command.Process.Release(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			if status.Signaled() {
				os.Exit(128 + int(status.Signal()))
			}
			os.Exit(status.ExitStatus())
		}
	}
}
