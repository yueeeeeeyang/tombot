package userapi

import (
	"context"
	"net"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/identity"
)

// 目录接口测试没有节点，意外访问桌面必须失败。
type unavailableConsole struct{}

func (unavailableConsole) Ready(context.Context, identity.Principal, string) error {
	return execution.ErrUnavailable
}
func (unavailableConsole) Stream(context.Context, identity.Principal, string) (net.Conn, error) {
	return nil, execution.ErrUnavailable
}

// 仅替代节点；真实 HTTPS 身份、租户查询、输入校验及目标回执仍经过正式实现。
type maintenanceNode struct{ computer.SessionRuntime }

func (maintenanceNode) ManageComputer(ctx context.Context, target computer.Maintenance) (computer.MaintenanceReceipt, error) {
	state := "stopped"
	if target.ExpectedVersion != "" {
		state = "restarting"
	}
	if target.Validate() != nil {
		return computer.MaintenanceReceipt{}, context.Canceled
	}
	return computer.MaintenanceReceipt{Target: target, ComputerStatus: computer.ComputerStatus{State: state, Version: target.ContainerID}}, ctx.Err()
}
