package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type consoleAuthority interface {
	consoleClient(context.Context, computer.SessionCommand, *processgroup.Reference) (computer.ConsoleRuntime, error)
}

func (link *agentLink) consoleClient(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.ConsoleRuntime, error) {
	return link.client(ctx, command, reference)
}

func (controller *SessionController) Console(ctx context.Context, request computer.ConsoleRequest) (receipt computer.ConsoleReceipt, err error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	err = controller.withOperation(ctx, request.Session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(consoleAuthority)
		if !ok {
			return failure("agent_console_unavailable")
		}
		client, err := authority.consoleClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		if client == nil {
			return failure("agent_console_unavailable")
		}
		receipt, err = client.Console(ctx, request)
		return err
	})
	return receipt, err
}
