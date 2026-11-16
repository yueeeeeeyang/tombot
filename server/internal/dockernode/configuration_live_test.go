//go:build dockerintegration

package dockernode

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

// TestLiveCreatedContainerConfiguration 只创建并核对本轮空资源，不启动容器或伪造能力验收。
func TestLiveCreatedContainerConfiguration(t *testing.T) {
	socket, image := os.Getenv("TOM_TEST_DOCKER_SOCKET"), os.Getenv("TOM_TEST_DOCKER_IMAGE")
	if socket == "" || image == "" {
		t.Fatal("必须指定测试 Docker 套接字和已有镜像")
	}
	engine, err := newEngine(socket, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.close)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	fixture := newFixture(t)
	description, err := engine.inspectImage(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	// 本测试使用 Docker 已解析的固定镜像 ID，只核验创建配置，不声明发布镜像符合节点协议。
	fixture.specification.ImageReference = description.ID
	resource := fixture.node.resource(fixture.specification)
	backing := resource.Name + "-test-storage"
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		container, err := engine.inspectContainer(cleanup, resource.Name)
		if err == nil {
			if !sameLabels(container.Config.Labels, containerLabels(resource)) || container.State.Running {
				t.Error("测试容器身份或停止状态不符")
				return
			}
			if err = engine.removeContainer(cleanup, container.ID); err != nil {
				t.Error(err)
				return
			}
		} else if !errors.Is(err, errMissing) {
			t.Error(err)
			return
		}
		for _, name := range []string{resource.Name, backing} {
			volume, err := engine.inspectVolume(cleanup, name)
			if errors.Is(err, errMissing) {
				continue
			}
			if err != nil || !sameLabels(volume.Labels, resource.Labels) {
				t.Error("测试卷身份不符", err)
				return
			}
			if err = engine.removeVolume(cleanup, name); err != nil {
				t.Error(err)
				return
			}
		}
	})
	// 独立空卷提供守护进程内路径，避免 Docker Desktop 改写 macOS 绑定路径；不启动容器。
	var storage struct{ Name, Mountpoint string }
	if err = engine.request(ctx, "POST", "/volumes/create", map[string]any{"Name": backing, "Driver": "local", "Labels": resource.Labels}, &storage, 201); err != nil {
		t.Fatal(err)
	}
	if storage.Name != backing || storage.Mountpoint == "" {
		t.Fatal("测试卷身份未确认")
	}
	resource.ControlPath, resource.WorkspacePath = storage.Mountpoint, storage.Mountpoint
	if _, err = engine.createVolume(ctx, resource.Name, resource.WorkspacePath, resource.Labels); err != nil {
		t.Fatal(err)
	}
	id, err := engine.createContainer(ctx, resource.Name, fixture.node.configuration(resource))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := engine.inspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	vol, err := engine.inspectVolume(ctx, resource.Name)
	if err != nil {
		t.Fatal(err)
	}
	current := snapshot{volume: vol, container: actual, hasVolume: true, hasContainer: true, workspace: Workspace{Exists: true, Path: resource.WorkspacePath, CapacityBytes: resource.Specification.Resources.DiskBytes}}
	if err = fixture.node.validateSnapshot(resource, &record{}, current); err != nil {
		var failure *computer.Failure
		if errors.As(err, &failure) {
			t.Fatalf("真实创建配置被拒绝：%s", failure.Code)
		}
		t.Fatal(err)
	}
}
