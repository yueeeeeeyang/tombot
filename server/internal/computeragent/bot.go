package computeragent

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type botScope interface {
	StartAgent(context.Context, processgroup.Reference, computer.ExecutionSession, *os.File, *os.File) error
	WriteAgent(context.Context, computer.AgentWrite) (int64, error)
	ReadAgent(context.Context, computer.AgentRead) (computer.AgentOutput, error)
}

func (controller *Controller) withBot(ctx context.Context, session computer.ExecutionSession, action func(context.Context, *desktopGuard, botScope) error) error {
	return controller.withScope(ctx, session, func(ctx context.Context, guard *desktopGuard) error {
		scope, ok := guard.scope.(botScope)
		if !ok {
			return errors.New("电脑代理未提供 Bot Agent 执行能力")
		}
		return action(ctx, guard, scope)
	})
}

func (controller *Controller) StartAgent(ctx context.Context, session computer.ExecutionSession) error {
	return controller.withBot(ctx, session, func(ctx context.Context, guard *desktopGuard, scope botScope) (failure error) {
		membership, err := guard.scope.Membership("shell")
		if err != nil {
			return err
		}
		defer func() {
			if membership.Close() != nil {
				failure = errors.Join(failure, errors.New("关闭 Bot Agent 入组描述符失败"))
			}
		}()
		return scope.StartAgent(ctx, *guard.state.Group, session, membership, guard.permit.reader)
	})
}

func (controller *Controller) WriteAgent(ctx context.Context, request computer.AgentWrite) (offset int64, failure error) {
	if err := request.Validate(); err != nil {
		return 0, err
	}
	failure = controller.withBot(ctx, request.Session, func(ctx context.Context, _ *desktopGuard, scope botScope) (err error) {
		offset, err = scope.WriteAgent(ctx, request)
		return err
	})
	return offset, failure
}

func (controller *Controller) ReadAgent(ctx context.Context, request computer.AgentRead) (output computer.AgentOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withBot(ctx, request.Session, func(ctx context.Context, _ *desktopGuard, scope botScope) (err error) {
		output, err = scope.ReadAgent(ctx, request)
		return err
	})
	return output, failure
}
