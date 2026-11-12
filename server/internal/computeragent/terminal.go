package computeragent

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type terminalScope interface {
	StartTerminal(context.Context, processgroup.Reference, computer.TerminalStart, *os.File, *os.File) (computer.TerminalOutput, error)
	ReadTerminal(context.Context, computer.TerminalRead) (computer.TerminalOutput, error)
	WriteTerminal(context.Context, computer.TerminalWrite) (computer.TerminalReceipt, error)
	ResizeTerminal(context.Context, computer.TerminalResize) error
}

func (controller *Controller) withTerminal(ctx context.Context, session computer.ExecutionSession, action func(context.Context, *desktopGuard, terminalScope) error) error {
	return controller.withScope(ctx, session, func(ctx context.Context, guard *desktopGuard) error {
		scope, ok := guard.scope.(terminalScope)
		if !ok {
			return errors.New("当前电脑代理未提供终端能力")
		}
		return action(ctx, guard, scope)
	})
}

func (controller *Controller) StartTerminal(ctx context.Context, request computer.TerminalStart) (output computer.TerminalOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, guard *desktopGuard, scope terminalScope) (err error) {
		membership, err := guard.scope.Membership("shell")
		if err != nil {
			return err
		}
		defer func() {
			if membership.Close() != nil {
				err = errors.Join(err, errors.New("关闭终端入组描述符失败"))
			}
		}()
		output, err = scope.StartTerminal(ctx, *guard.state.Group, request, membership, guard.permit.reader)
		return err
	})
	return output, failure
}

func (controller *Controller) ReadTerminal(ctx context.Context, request computer.TerminalRead) (output computer.TerminalOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, _ *desktopGuard, scope terminalScope) (err error) {
		output, err = scope.ReadTerminal(ctx, request)
		return err
	})
	return output, failure
}

func (controller *Controller) WriteTerminal(ctx context.Context, request computer.TerminalWrite) (receipt computer.TerminalReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, _ *desktopGuard, scope terminalScope) (err error) {
		receipt, err = scope.WriteTerminal(ctx, request)
		return err
	})
	return receipt, failure
}

func (controller *Controller) ResizeTerminal(ctx context.Context, request computer.TerminalResize) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return controller.withTerminal(ctx, request.Session, func(ctx context.Context, _ *desktopGuard, scope terminalScope) error {
		return scope.ResizeTerminal(ctx, request)
	})
}
