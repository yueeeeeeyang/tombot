package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/computer"
)

func (controller *Controller) withDesktopIO(ctx context.Context, session computer.ExecutionSession, action func(context.Context, computer.DesktopIORuntime) error) error {
	return controller.withScope(ctx, session, func(ctx context.Context, guard *desktopGuard) error {
		scope, ok := guard.scope.(computer.DesktopIORuntime)
		if !ok {
			return errors.New("当前电脑代理未提供桌面输入输出能力")
		}
		return action(ctx, scope)
	})
}

func (controller *Controller) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (result computer.DesktopImage, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktopIO(ctx, request.Session, func(ctx context.Context, scope computer.DesktopIORuntime) (err error) {
		result, err = scope.CaptureDesktop(ctx, request)
		return err
	})
	return result, failure
}

func (controller *Controller) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (result computer.DesktopImageChunk, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktopIO(ctx, request.Session, func(ctx context.Context, scope computer.DesktopIORuntime) (err error) {
		result, err = scope.ReadDesktopImage(ctx, request)
		return err
	})
	return result, failure
}

func (controller *Controller) InputDesktop(ctx context.Context, request computer.DesktopInput) (result computer.DesktopInputReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktopIO(ctx, request.Session, func(ctx context.Context, scope computer.DesktopIORuntime) (err error) {
		result, err = scope.InputDesktop(ctx, request)
		return err
	})
	return result, failure
}
