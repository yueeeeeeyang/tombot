package dockernode

import (
	"context"
	"math"
	"sync/atomic"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

const runtimeRecordName = "node-runtime.json"

type runtimeRecord struct {
	Version    int    `json:"version"`
	NodeID     string `json:"node_id"`
	InstanceID string `json:"instance_id"`
	Generation int64  `json:"generation"`
}
type runtimeHeartbeat struct {
	record   runtimeRecord
	sequence atomic.Int64
}

// newHeartbeat 在生命周期独占锁内先刷盘新代次，启动恢复失败也不复用该代次。
func newHeartbeat(journal *journal, nodeID string) (*runtimeHeartbeat, error) {
	encoded, err := journal.readFile(runtimeRecordName)
	if err != nil {
		return nil, err
	}
	previous := runtimeRecord{}
	if encoded != nil {
		if agentchannel.Decode(encoded, &previous) != nil || previous.Version != 1 || (computer.NodeHeartbeat{NodeID: previous.NodeID, InstanceID: previous.InstanceID, Generation: previous.Generation, Sequence: 1}).Validate(computer.NodeQuery{NodeID: nodeID}) != nil {
			return nil, failure("node_runtime_record_invalid")
		}
	}
	if previous.Generation == math.MaxInt64 {
		return nil, failure("node_runtime_generation_exhausted")
	}
	current := runtimeRecord{Version: 1, NodeID: nodeID, InstanceID: ids.New(), Generation: previous.Generation + 1}
	encoded, err = agentchannel.Encode(current)
	if err != nil {
		return nil, err
	}
	if err := journal.writeFile(runtimeRecordName, encoded); err != nil {
		return nil, err
	}
	return &runtimeHeartbeat{record: current}, nil
}

// Heartbeat 仅返回控制进程代次；它不执行创建、不重放任务，也不会把心跳提升为能力通过。
func (node *Runtime) Heartbeat(ctx context.Context, query computer.NodeQuery) (computer.NodeHeartbeat, error) {
	if err := ctx.Err(); err != nil {
		return computer.NodeHeartbeat{}, err
	}
	if query.Validate() != nil || query.NodeID != node.options.NodeID || node.heartbeat == nil {
		return computer.NodeHeartbeat{}, failure("node_heartbeat_invalid")
	}
	for {
		previous := node.heartbeat.sequence.Load()
		if previous == math.MaxInt64 {
			return computer.NodeHeartbeat{}, failure("node_heartbeat_sequence_exhausted")
		}
		if node.heartbeat.sequence.CompareAndSwap(previous, previous+1) {
			record := node.heartbeat.record
			return computer.NodeHeartbeat{NodeID: record.NodeID, InstanceID: record.InstanceID, Generation: record.Generation, Sequence: previous + 1}, nil
		}
	}
}
