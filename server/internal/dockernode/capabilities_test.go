package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

func TestCapabilityProbeRequiresCurrentClosedEvidence(t *testing.T) {
	for _, scenario := range []string{"partial", "unclosed", "replayed", "lease", "lifecycle", "container", "parent", "storage", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f, probes, _, resource, current := newProbeFixture(t)
			evidence, err := probes.check(t.Context(), resource, current)
			if err != nil {
				t.Fatal(err)
			}
			probe := &capabilityProbe{node: f.node, locate: probes.locate}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "unclosed":
				state, err := f.node.journal.readProbe(resource.Specification.UserID)
				if err != nil {
					t.Fatal(err)
				}
				state.Phase = "closing"
				encoded, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				// 模拟进程在 closing 持久化之后退出；正常状态机不允许关闭记录倒退。
				if err := f.node.journal.writeFile(resource.Specification.UserID+".probe.json", encoded); err != nil {
					t.Fatal(err)
				}
			case "replayed":
				evidence.ServiceID = ids.New()
			case "lease":
				resource.Specification.FencingToken++
			case "lifecycle":
				state, err := f.node.journal.read(resource.Specification.UserID)
				if err != nil {
					t.Fatal(err)
				}
				state.Phase = "removing"
				if err := f.node.journal.save(state); err != nil {
					t.Fatal(err)
				}
			case "container":
				current.ID = "changed"
			case "parent":
				probe.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
					parent, err := probes.locate(ctx, id, pid)
					parent.Inode++
					return parent, err
				}
			case "storage":
				f.storage.verifyError = errors.New("测试挂载已改变")
			case "cancelled":
				cancel()
			}
			capabilities, err := probe.Check(ctx, resource, current.ID, evidence)
			if scenario == "partial" {
				if err != nil || !capabilities.Terminal || !capabilities.Files || !capabilities.ProcessReaping || !capabilities.DiskQuota || !capabilities.NetworkIsolation || capabilities.Browser || capabilities.Ready() {
					t.Fatal("基础证据错误或被提升为完整就绪", capabilities, err)
				}
			} else if err == nil || capabilities != (computer.Capabilities{}) {
				t.Fatal("未确认的证据没有封闭能力", capabilities, err)
			}
		})
	}
}

func TestStorageVerifiesActualProbeFilesystem(t *testing.T) {
	for _, scenario := range []string{"match", "configured_capacity", "type", "zero", "mount", "device", "statfs", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStorageFixture(t)
			if _, err := f.storage.ensure(t.Context(), f.resource); err != nil {
				t.Fatal(err)
			}
			size := uint64(f.resource.Specification.Resources.DiskBytes - 4096)
			kind := int64(0xef53)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario {
			case "configured_capacity":
				size += 4096
			case "type":
				kind = 0x794c7630
			case "zero":
				size = 0
			case "mount":
				f.mounted = nil
			case "device":
				f.devices[0].BackingInode++
			case "statfs":
				f.storage.filesystemSize = func(string) (int64, error) { return 0, errors.New("测试容量读取失败") }
			case "cancelled":
				cancel()
			}
			_, err := f.storage.verifyFilesystem(ctx, f.resource, kind, size)
			if (err == nil) != (scenario == "match") {
				t.Fatal(scenario, err)
			}
		})
	}
}
