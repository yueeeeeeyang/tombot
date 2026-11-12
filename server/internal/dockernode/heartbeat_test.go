package dockernode

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestHeartbeatPersistentGenerationAndConcurrentSequence(t *testing.T) {
	f := newFixture(t)
	var previous computer.NodeHeartbeat
	for generation := int64(1); generation <= 2; generation++ {
		var err error
		f.node.heartbeat, err = newHeartbeat(f.node.journal, f.node.options.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		results := make(chan computer.NodeHeartbeat, 32)
		failures := make(chan error, 32)
		var waiting sync.WaitGroup
		for range 32 {
			waiting.Go(func() {
				heartbeat, err := f.node.Heartbeat(t.Context(), computer.NodeQuery{NodeID: f.node.options.NodeID})
				results <- heartbeat
				failures <- err
			})
		}
		waiting.Wait()
		close(results)
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		seen := map[int64]bool{}
		for heartbeat := range results {
			if heartbeat.Generation != generation || heartbeat.Validate(computer.NodeQuery{NodeID: f.node.options.NodeID}) != nil || seen[heartbeat.Sequence] || heartbeat.Sequence > 32 {
				t.Fatal("代次或并发序号错误", heartbeat)
			}
			seen[heartbeat.Sequence] = true
			if generation > 1 && heartbeat.InstanceID == previous.InstanceID {
				t.Fatal("重启复用进程身份")
			}
		}
		previous, err = f.node.Heartbeat(t.Context(), computer.NodeQuery{NodeID: f.node.options.NodeID})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.node.Heartbeat(ctx, computer.NodeQuery{NodeID: f.node.options.NodeID}); err == nil {
		t.Fatal("取消后仍发布心跳")
	}
	if _, err := f.node.Heartbeat(t.Context(), computer.NodeQuery{NodeID: ids.New()}); err == nil {
		t.Fatal("接受其他节点身份")
	}
	f.node.heartbeat.sequence.Store(math.MaxInt64)
	if _, err := f.node.Heartbeat(t.Context(), computer.NodeQuery{NodeID: f.node.options.NodeID}); err == nil {
		t.Fatal("序号耗尽后回绕")
	}
}

func TestHeartbeatRejectsUncertainPersistentIdentity(t *testing.T) {
	for _, scenario := range []string{"node", "version", "generation", "overflow", "permission", "link"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			heartbeat, err := newHeartbeat(f.node.journal, f.node.options.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			state := heartbeat.record
			switch scenario {
			case "node":
				state.NodeID = ids.New()
			case "version":
				state.Version++
			case "generation":
				state.Generation = 0
			case "overflow":
				state.Generation = math.MaxInt64
			}
			encoded, err := agentchannel.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.node.journal.writeFile(runtimeRecordName, encoded); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.statePath, runtimeRecordName)
			if scenario == "permission" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "link" {
				if err := os.Link(path, filepath.Join(f.statePath, "linked")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := newHeartbeat(f.node.journal, f.node.options.NodeID); err == nil {
				t.Fatal("不确定代次被覆盖")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(encoded) {
				t.Fatal("失败覆盖了原恢复记录", err)
			}
		})
	}
}
