//go:build linux

package processgroup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io.yueyang/tom/internal/infra/ids"
	"os"
	"path/filepath"
	"testing"
)

// TestServiceDrainAbsentParent 只读核实随机父组不存在，不创建、写入或删除宿主资源组。
func TestServiceDrainAbsentParent(t *testing.T) {
	reference := serviceFixture()
	reference.Container.ContainerID = fmt.Sprintf("%x", sha256.Sum256([]byte(ids.New())))
	reference.Container.Path = "system.slice/docker-" + reference.Container.ContainerID + ".scope"
	services := &serviceController{groups: &controller{}}
	if err := services.Drain(t.Context(), reference); err != nil {
		t.Fatal("内核已移除父组仍不能封闭旧实例", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := services.Drain(ctx, reference); !errors.Is(err, context.Canceled) {
		t.Fatal("取消被当作父组已消失", err)
	}
}

func TestServiceOperationsRejectInvalidIdentity(t *testing.T) {
	services := &serviceController{groups: &controller{}}
	reference := serviceFixture()
	reference.Container.Inode = 0
	ctx := context.Background()
	if services.Create(ctx, reference) == nil || services.Drain(ctx, reference) == nil {
		t.Fatal("错误身份进入资源组变更")
	}
	if files, err := services.Controls(ctx, reference); err == nil || len(files) != 0 {
		t.Fatal("错误身份获得控制入口")
	}
	if member, err := services.Contains(ctx, reference, os.Getpid()); member || err == nil {
		t.Fatal("错误身份被视为服务进程")
	}
	for _, pid := range []int{-1, 0, 1} {
		if member, err := services.Contains(ctx, serviceFixture(), pid); member || err == nil {
			t.Fatal("无效进程被视为服务进程")
		}
	}
}

func TestServiceOperationsHonorCancellation(t *testing.T) {
	services := &serviceController{groups: &controller{}}
	reference := serviceFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(services.Create(ctx, reference), context.Canceled) || !errors.Is(services.Drain(ctx, reference), context.Canceled) {
		t.Fatal("取消调用仍尝试变更服务资源组")
	}
	if files, err := services.Controls(ctx, reference); len(files) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("取消调用仍发放服务控制入口", err)
	}
	if member, err := services.Contains(ctx, reference, os.Getpid()); member || !errors.Is(err, context.Canceled) {
		t.Fatal("取消调用仍确认进程身份", err)
	}
}

func TestServiceControlsRejectSubstitutes(t *testing.T) {
	reference := serviceFixture()
	if ValidateServiceControls(reference, nil) == nil || ValidateServiceControls(reference, make([]*os.File, 3)) == nil || ValidateServiceControls(reference, make([]*os.File, 4)) == nil {
		t.Fatal("接受缺失、空白或任务会话数量的控制入口")
	}
	var files []*os.File
	t.Cleanup(func() {
		for _, file := range files {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	directory := t.TempDir()
	for index, name := range []string{"cgroup.procs", "cgroup.kill", "cgroup.events"} {
		filename := filepath.Join(directory, name)
		if err := os.WriteFile(filename, []byte("populated 0\n"), 0600); err != nil {
			t.Fatal(err)
		}
		mode := os.O_WRONLY
		if index == 2 {
			mode = os.O_RDONLY
		}
		file, err := os.OpenFile(filename, mode, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if ValidateServiceControls(reference, files) == nil {
		t.Fatal("普通文件被当成内核控制入口")
	}
	// 校验只借用描述符，失败不能关闭调用方资源。
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatal("拒绝后错误关闭调用方入口", err)
		}
	}
}
