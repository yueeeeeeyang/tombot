package computer

import (
	"context"
	"errors"

	"github.com/oklog/ulid/v2"
)

// NodeQuery 只查询固定节点控制进程，不接收宿主动作或能力声明。
type NodeQuery struct {
	NodeID string `json:"node_id"`
}

func (query NodeQuery) Validate() error {
	id, err := ulid.ParseStrict(query.NodeID)
	if err != nil || id.String() != query.NodeID {
		return errors.New("节点心跳身份无效")
	}
	return nil
}

// NodeHeartbeat 证明受保护控制服务当前可响应；不能证明 Docker、镜像或用户电脑能力就绪。
// Generation 持久跨进程递增，Sequence 仅在同一 InstanceID 内递增，不用墙上时间判断新旧。
type NodeHeartbeat struct {
	NodeID     string `json:"node_id"`
	InstanceID string `json:"instance_id"`
	Generation int64  `json:"generation"`
	Sequence   int64  `json:"sequence"`
}

func (heartbeat NodeHeartbeat) Validate(query NodeQuery) error {
	id, err := ulid.ParseStrict(heartbeat.InstanceID)
	if query.Validate() != nil || heartbeat.NodeID != query.NodeID || err != nil || id.String() != heartbeat.InstanceID || heartbeat.Generation < 1 || heartbeat.Sequence < 1 {
		return errors.New("节点心跳代次或归属无效")
	}
	return nil
}

type NodeRuntime interface {
	Heartbeat(context.Context, NodeQuery) (NodeHeartbeat, error)
}
