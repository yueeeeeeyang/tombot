package dockernode

import (
	"context"
	"reflect"
	"testing"

	computerimage "io.yueyang/tom/deploy/computer"
)

func TestDesktopRuntimeConfigurationAndDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*hostConfig)
		code   string
	}{
		{"旧临时目录权限", func(value *hostConfig) { value.Tmpfs["/tmp"] = "rw,nosuid,nodev,noexec,size=67108864,mode=0700" }, "container_runtime_mismatch"},
		{"旧运行目录权限", func(value *hostConfig) { value.Tmpfs["/run"] = "rw,nosuid,nodev,noexec,size=16777216,mode=0700" }, "container_runtime_mismatch"},
		{"遗漏浏览器策略", func(value *hostConfig) { value.SecurityOpt = []string{"no-new-privileges:true"} }, "container_privileges_mismatch"},
		{"不受限策略", func(value *hostConfig) { value.SecurityOpt = []string{"no-new-privileges:true", "seccomp=unconfined"} }, "container_privileges_mismatch"},
		{"共享内存漂移", func(value *hostConfig) { value.ShmSize++ }, "container_limits_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			if err := validateOptions(fixture.node.options); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.node.Ensure(context.Background(), fixture.specification); err != nil {
				t.Fatal(err)
			}
			name := fixture.node.resource(fixture.specification).Name
			value := fixture.engine.containers[name]
			if !reflect.DeepEqual(value.HostConfig.SecurityOpt, computerimage.SecurityOptions()) || value.HostConfig.ShmSize != fixture.node.options.SharedMemoryBytes || value.HostConfig.Tmpfs["/tmp"] != "rw,nosuid,nodev,noexec,size=67108864,mode=1777" || value.HostConfig.Tmpfs["/run"] != "rw,nosuid,nodev,noexec,size=134217728,mode=0755" {
				t.Fatal("节点未提供镜像要求的图形运行配置")
			}
			test.change(&value.HostConfig)
			fixture.engine.containers[name] = value
			before := len(fixture.engine.events)
			_, err := fixture.node.Inspect(context.Background(), fixture.specification)
			requireFailure(t, err, test.code)
			if len(fixture.engine.events) != before {
				t.Fatal("对账修复了未知漂移容器")
			}
		})
	}
}

func TestDesktopRuntimeRequiresBoundedMemorySettings(t *testing.T) {
	fixture := newFixture(t)
	for _, value := range []int64{0, (16 << 20) - 1, (4 << 30) + 1} {
		options := fixture.node.options
		options.RuntimeBytes = value
		if validateOptions(options) == nil {
			t.Fatal("图形运行空间未明确限制", value)
		}
	}
	for _, value := range []int64{0, (64 << 20) - 1, (4 << 30) + 1} {
		options := fixture.node.options
		options.SharedMemoryBytes = value
		if validateOptions(options) == nil {
			t.Fatal("图形共享内存未明确限制", value)
		}
	}
}
