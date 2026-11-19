package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
)

// Console 保持桌面原执行权，单独确认任务冻结后才允许用户获取画面和输入。
func (controller *Controller) Console(ctx context.Context, request computer.ConsoleRequest) (receipt computer.ConsoleReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	if request.Action == "prepare" {
		status, err := controller.InspectDesktop(ctx, request.Session)
		if err != nil {
			return receipt, err
		}
		if status.State != "ready" {
			return receipt, errors.New("观看桌面尚未就绪")
		}
		return computer.ConsoleReceipt{Session: request.Session, Revision: request.Revision, State: "ready"}, nil
	}
	failure = controller.withScope(ctx, request.Session, func(ctx context.Context, guard *desktopGuard) error {
		runtime, ok := guard.scope.(computer.ConsoleRuntime)
		if !ok {
			return errors.New("当前电脑代理未提供桌面接管")
		}
		var err error
		receipt, err = runtime.Console(ctx, request)
		return err
	})
	return receipt, failure
}
