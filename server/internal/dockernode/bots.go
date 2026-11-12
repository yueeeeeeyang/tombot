package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type botAuthority interface {
	agentClient(context.Context, computer.SessionCommand, *processgroup.Reference) (computer.AgentRuntime, error)
}

func (link *agentLink) agentClient(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.AgentRuntime, error) {
	return link.client(ctx, command, reference)
}

// withAgent 每块标准流读写都重新核对桌面执行权，避免长连接越过租约或阻塞续约。
func (controller *SessionController) withAgent(ctx context.Context, session computer.ExecutionSession, action func(context.Context, computer.AgentRuntime) error) error {
	return controller.withOperation(ctx, session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(botAuthority)
		if !ok {
			return failure("agent_runtime_unavailable")
		}
		client, err := authority.agentClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		if client == nil {
			return failure("agent_runtime_unavailable")
		}
		return action(ctx, client)
	})
}

func (controller *SessionController) StartAgent(ctx context.Context, session computer.ExecutionSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	return controller.withAgent(ctx, session, func(ctx context.Context, runtime computer.AgentRuntime) error {
		return runtime.StartAgent(ctx, session)
	})
}

func (controller *SessionController) WriteAgent(ctx context.Context, request computer.AgentWrite) (offset int64, failure error) {
	if err := request.Validate(); err != nil {
		return 0, err
	}
	failure = controller.withAgent(ctx, request.Session, func(ctx context.Context, runtime computer.AgentRuntime) (err error) {
		offset, err = runtime.WriteAgent(ctx, request)
		return err
	})
	return offset, failure
}

func (controller *SessionController) ReadAgent(ctx context.Context, request computer.AgentRead) (output computer.AgentOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withAgent(ctx, request.Session, func(ctx context.Context, runtime computer.AgentRuntime) (err error) {
		output, err = runtime.ReadAgent(ctx, request)
		return err
	})
	return output, failure
}
