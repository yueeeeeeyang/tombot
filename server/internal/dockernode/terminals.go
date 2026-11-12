package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type terminalAuthority interface {
	terminalClient(context.Context, computer.SessionCommand, *processgroup.Reference) (computer.TerminalRuntime, error)
}

func (link *agentLink) terminalClient(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.TerminalRuntime, error) {
	return link.client(ctx, command, reference)
}

// withTerminal 持有生命周期共享锁和目标桌面锁，沿用现有期限，不因读写操作延长租约。
func (controller *SessionController) withTerminal(ctx context.Context, session computer.ExecutionSession, action func(context.Context, computer.TerminalRuntime) error) error {
	return controller.withOperation(ctx, session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(terminalAuthority)
		if !ok {
			return failure("agent_terminal_unavailable")
		}
		client, err := authority.terminalClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		if client == nil {
			return failure("agent_terminal_unavailable")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return action(ctx, client)
	})
}

func (controller *SessionController) StartTerminal(ctx context.Context, request computer.TerminalStart) (output computer.TerminalOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, client computer.TerminalRuntime) (err error) {
		output, err = client.StartTerminal(ctx, request)
		return err
	})
	return output, failure
}

func (controller *SessionController) ReadTerminal(ctx context.Context, request computer.TerminalRead) (output computer.TerminalOutput, failure error) {
	if err := request.Validate(); err != nil {
		return output, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, client computer.TerminalRuntime) (err error) {
		output, err = client.ReadTerminal(ctx, request)
		return err
	})
	return output, failure
}

func (controller *SessionController) WriteTerminal(ctx context.Context, request computer.TerminalWrite) (receipt computer.TerminalReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	failure = controller.withTerminal(ctx, request.Session, func(ctx context.Context, client computer.TerminalRuntime) (err error) {
		receipt, err = client.WriteTerminal(ctx, request)
		return err
	})
	return receipt, failure
}

func (controller *SessionController) ResizeTerminal(ctx context.Context, request computer.TerminalResize) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return controller.withTerminal(ctx, request.Session, func(ctx context.Context, client computer.TerminalRuntime) error {
		return client.ResizeTerminal(ctx, request)
	})
}
