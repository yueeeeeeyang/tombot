package computeragent

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type desktopScope interface {
	StartDesktop(context.Context, processgroup.Reference, computer.ExecutionSession, *os.File, *os.File, func(string)) (computer.DesktopStatus, error)
	InspectDesktop(context.Context, computer.ExecutionSession) (computer.DesktopStatus, error)
}

func (controller *Controller) StartDesktop(ctx context.Context, session computer.ExecutionSession) (status computer.DesktopStatus, failure error) {
	failure = controller.withScope(ctx, session, func(ctx context.Context, guard *desktopGuard) (err error) {
		scope, ok := guard.scope.(desktopScope)
		if !ok {
			return errors.New("当前电脑代理未提供图形会话能力")
		}
		membership, err := guard.scope.Membership("shell")
		if err != nil {
			return err
		}
		defer func() {
			if membership.Close() != nil {
				err = errors.Join(err, errors.New("关闭图形会话入组描述符失败"))
			}
		}()
		permit := guard.permit
		status, err = scope.StartDesktop(ctx, *guard.state.Group, session, membership, permit.reader, func(code string) {
			if permit.revoke() != nil {
				guard.fail()
			}
			guard.logger.Error("图形会话失败，撤销所属桌面执行授权", "user_id", session.UserID, "computer_id", session.ComputerID, "desktop_id", session.DesktopID, "session_id", session.SessionID, "error_code", code)
		})
		return err
	})
	return status, failure
}

func (controller *Controller) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (status computer.DesktopStatus, failure error) {
	failure = controller.withScope(ctx, session, func(ctx context.Context, guard *desktopGuard) (err error) {
		scope, ok := guard.scope.(desktopScope)
		if !ok {
			return errors.New("当前电脑代理未提供图形会话能力")
		}
		status, err = scope.InspectDesktop(ctx, session)
		return err
	})
	return status, failure
}
