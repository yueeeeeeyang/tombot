package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type fileAuthority interface {
	fileClient(context.Context, computer.SessionCommand, *processgroup.Reference) (computer.FileRuntime, error)
}

func (link *agentLink) fileClient(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.FileRuntime, error) {
	return link.client(ctx, command, reference)
}

func (controller *SessionController) TransferFile(ctx context.Context, request computer.FileRequest) (receipt computer.FileReceipt, err error) {
	if err := request.Validate(); err != nil {
		return receipt, err
	}
	err = controller.withOperation(ctx, request.Session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(fileAuthority)
		if !ok {
			return failure("agent_files_unavailable")
		}
		client, err := authority.fileClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		if client == nil {
			return failure("agent_files_unavailable")
		}
		receipt, err = client.TransferFile(ctx, request)
		return err
	})
	return receipt, err
}
