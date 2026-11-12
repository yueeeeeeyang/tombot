//go:build dockerintegration

package dockernode

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// TestLiveDockerLifecycle 只创建本次测试专属资源；验证实际 API 和删除顺序，不宣称电脑能力已就绪。
func TestLiveDockerLifecycle(t *testing.T) {
	socket, image := os.Getenv("TOM_TEST_DOCKER_SOCKET"), os.Getenv("TOM_TEST_DOCKER_IMAGE")
	if socket == "" || !strings.Contains(image, "@sha256:") {
		t.Fatal("真实节点测试必须指定本机套接字和已存在的固定摘要 Alpine 镜像")
	}
	engine, err := newEngine(socket, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := "tombot-lifecycle-test-" + strings.ToLower(ids.New())
	backing := name + "-storage"
	labels := map[string]string{"io.tombot.test": name}
	var containerID string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		{
			value, err := engine.inspectContainer(cleanup, name)
			if err != nil && !errors.Is(err, errMissing) {
				t.Error("测试容器状态无法确认", err)
				return
			}
			if err == nil {
				if !sameLabels(value.Config.Labels, labels) {
					t.Error("测试容器所属关系不符，拒绝清理")
					return
				}
				if err = engine.stopContainer(cleanup, value.ID); err != nil && !errors.Is(err, errMissing) {
					t.Error(err)
					return
				}
				if err = engine.removeContainer(cleanup, value.ID); err != nil && !errors.Is(err, errMissing) {
					t.Error(err)
					return
				}
			}
		}
		for _, volumeName := range []string{name, backing} {
			value, err := engine.inspectVolume(cleanup, volumeName)
			if errors.Is(err, errMissing) {
				continue
			}
			if err != nil || !sameLabels(value.Labels, labels) {
				t.Error("测试卷状态或所属关系无法确认", err)
				return
			}
			if err = engine.removeVolume(cleanup, volumeName); err != nil && !errors.Is(err, errMissing) {
				t.Error(err)
				return
			}
		}
	})
	// 独立测试卷提供空白宿主存储，不读取或挂载开发者既有卷和目录；它不代表正式硬配额后端。
	var backingVolume struct{ Name, Mountpoint string }
	if err = engine.request(ctx, "POST", "/volumes/create", map[string]any{"Name": backing, "Driver": "local", "Labels": labels}, &backingVolume, 201); err != nil {
		t.Fatal(err)
	}
	if backingVolume.Name != backing || backingVolume.Mountpoint == "" {
		t.Fatal("新测试卷身份无效")
	}
	volume, err := engine.createVolume(ctx, name, backingVolume.Mountpoint, labels)
	if err != nil {
		t.Fatal(err)
	}
	if volume.Name != name || volume.Options["device"] != backingVolume.Mountpoint || !sameLabels(volume.Labels, labels) {
		t.Fatal("实际卷配置不符")
	}
	// 先核验生产容器配置在 Docker 中的实际规范形式；测试镜像没有电脑代理，因此此实例只创建、不启动。
	profileNode := &Runtime{options: Options{TemporaryBytes: 64 << 20, RuntimeBytes: 16 << 20, SharedMemoryBytes: 64 << 20}}
	profileResource := Resource{Name: name, ControlPath: backingVolume.Mountpoint, Labels: labels, Specification: computer.Specification{ImageReference: image, ComputerID: ids.New(), Resources: computer.Resources{CPUMillis: 500, MemoryBytes: 128 << 20, DiskBytes: 512 << 20, ProcessLimit: 32}}}
	containerID, err = engine.createContainer(ctx, name, profileNode.configuration(profileResource))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := engine.inspectContainer(ctx, containerID)
	if err != nil {
		t.Fatal(err)
	}
	if err = profileNode.validateContainer(profileResource, profile); err != nil {
		t.Fatal("Docker 实际保存的生产配置不符合节点校验", err)
	}
	if err = engine.removeContainer(ctx, containerID); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.inspectContainer(ctx, containerID); !errors.Is(err, errMissing) {
		t.Fatal("未启动的配置检查实例未清理", err)
	}
	init := true
	configuration := createContainer{Image: image, User: "65534:65534", Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", `test "$(id -u)" = 65534 && test ! -w / && test -d /workspace && test "$(ls /sys/class/net)" = lo && exec sleep 120`}, Labels: labels}
	configuration.HostConfig = hostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, NanoCpus: 500000000, Memory: 128 << 20, MemorySwap: 128 << 20, PidsLimit: 32, Init: &init, IpcMode: "private", CgroupnsMode: "private", Mounts: []mount{{Type: "volume", Source: name, Target: "/workspace", VolumeOptions: &volumeMountOptions{NoCopy: true}}}}
	containerID, err = engine.createContainer(ctx, name, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.startContainer(ctx, containerID); err != nil {
		t.Fatal(err)
	}
	actual, err := engine.inspectContainer(ctx, containerID)
	if err != nil {
		t.Fatal(err)
	}
	if !actual.State.Running || actual.Config.User != "65534:65534" || !actual.HostConfig.ReadonlyRootfs || actual.HostConfig.Privileged || actual.HostConfig.PidsLimit != 32 || !sameLabels(actual.Config.Labels, labels) {
		t.Fatal("实际容器资源与隔离配置不符")
	}
	if err = validateNetwork(actual); err != nil || actual.HostConfig.NetworkMode != "none" {
		t.Fatal("实际电脑存在可路由网络", err)
	}
	if err = engine.removeVolume(ctx, name); err == nil {
		t.Fatal("Docker 允许删除仍被容器占用的卷")
	}
	if err = engine.stopContainer(ctx, containerID); err != nil {
		t.Fatal(err)
	}
	actual, err = engine.inspectContainer(ctx, containerID)
	if err != nil || actual.State.Running {
		t.Fatal("实际容器停止未确认", err)
	}
	if err = engine.removeContainer(ctx, containerID); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.inspectContainer(ctx, containerID); !errors.Is(err, errMissing) {
		t.Fatal("容器删除未确认", err)
	}
	if err = engine.removeVolume(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.inspectVolume(ctx, name); !errors.Is(err, errMissing) {
		t.Fatal("卷删除未确认", err)
	}
	t.Log("真实 Docker 创建、非 root 运行、配额配置、占用卷拒删、停止及删除已验证；测试资源由清理器核实回收")
}
