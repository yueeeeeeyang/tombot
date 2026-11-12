//go:build linux && taskintegration && !cgo

package taskprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 这组测试只在新建的受限容器内执行，不创建宿主资源组，也不替代真实入组验收。
func requireIsolationContainer(t *testing.T) {
	t.Helper()
	if os.Getenv("TOM_TASK_ISOLATION_TEST") != "1" {
		t.Fatal("必须显式启用独立容器任务隔离验收")
	}
	if err := requireController(); err != nil {
		t.Fatal(err)
	}
}

func TestKernelRunRejectsFakeMembershipBeforeUserCode(t *testing.T) {
	requireIsolationContainer(t)
	request := validRequest()
	prepareDirectories(t, &request)
	request.Arguments = []string{"-c", "printf unexpected > '" + request.WorkingDirectory + "/should-not-exist'"}
	output, err := runChild(t, request, "run")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(output), "任务入组描述符不是受管内核入口") {
		t.Fatalf("伪造资源组未阻断：%v\n%s", err, output)
	}
	if _, err := os.Stat(request.WorkingDirectory + "/should-not-exist"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("入组失败后执行了用户代码", err)
	}
}

func TestKernelRevocationBlocksLateBootstrap(t *testing.T) {
	requireIsolationContainer(t)
	request := validRequest()
	prepareDirectories(t, &request)
	request.Arguments = []string{"-c", "printf unexpected > '" + request.WorkingDirectory + "/should-not-exist'"}
	output, err := runChild(t, request, "revoked")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(output), "任务执行授权已撤销") {
		t.Fatalf("已撤销引导未阻断：%v\n%s", err, output)
	}
	if _, err := os.Stat(request.WorkingDirectory + "/should-not-exist"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("撤销后执行了用户代码", err)
	}
}

func runChild(t *testing.T, request Request, phase string) ([]byte, error) {
	t.Helper()
	file, err := Seal(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	control, err := os.CreateTemp(t.TempDir(), "controller-only")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := control.WriteString("controller-private-file"); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	if phase == "revoked" {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		defer func() {
			if err := writer.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestTaskProcessChild$")
	command.Env = []string{"TOM_TASK_CHILD=" + phase, "PARENT_ONLY=controller-only"}
	command.ExtraFiles = []*os.File{file, control, reader}
	return command.CombinedOutput()
}

// TestTaskProcessChild 仅存在于测试二进制；直接验证降权阶段，不给正式引导提供跳过入组的开关。
func TestTaskProcessChild(t *testing.T) {
	phase := os.Getenv("TOM_TASK_CHILD")
	if phase == "" {
		return
	}
	if err := requireController(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	if phase == "run" {
		fmt.Fprintln(os.Stderr, Run())
		os.Exit(125)
	}
	if phase != "drop" && phase != "revoked" {
		os.Exit(125)
	}
	file := os.NewFile(3, "task-request")
	request, err := readSealed(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintln(os.Stderr, "读取测试任务失败")
		os.Exit(125)
	}
	if err = verifyDesktopDirectory(request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	// 多个已存在线程用于覆盖纯 Go 全线程身份修改，不仅验证当前调用线程。
	ready := make(chan struct{}, 4)
	for range 4 {
		go func() { runtime.LockOSThread(); ready <- struct{}{}; select {} }()
	}
	for range 4 {
		<-ready
	}
	fmt.Fprintln(os.Stderr, execute(request, os.NewFile(5, "task-revocation")))
	os.Exit(125)
}
