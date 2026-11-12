package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
)

// withScope 对每一次电脑操作重新检查完整会话；独立过期撤销不等待桌面锁。
func (controller *Controller) withScope(ctx context.Context, session computer.ExecutionSession, action func(context.Context, *desktopGuard) error) error {
	ctx, leave, err := controller.begin(ctx)
	if err != nil {
		return err
	}
	defer leave()
	if session.Validate() != nil || !controller.identity.matches(session) {
		return errors.New("电脑桌面执行身份无效")
	}
	guard, err := controller.desktop(ctx, session.DesktopIdentity, false)
	if err != nil {
		return err
	}
	call, unlock, err := guard.enter(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if guard.stopped || guard.state == nil || guard.state.Command.Session != session || guard.state.Phase != "active" || guard.scope == nil || guard.permit == nil || !guard.permit.active(guard.state.Command.Revision) {
		return errors.New("电脑操作没有有效桌面授权")
	}
	err = action(call, guard)
	if !guard.permit.active(guard.state.Command.Revision) {
		return errors.Join(err, errors.New("电脑操作期间授权已结束，结果需要对账"))
	}
	return err
}
