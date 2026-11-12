package dockernode

import (
	"context"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
)

type graphicsAuthority interface {
	desktopClient(context.Context, computer.SessionCommand, *processgroup.Reference) (computer.DesktopRuntime, error)
}

func (link *agentLink) desktopClient(ctx context.Context, command computer.SessionCommand, reference *processgroup.Reference) (computer.DesktopRuntime, error) {
	return link.client(ctx, command, reference)
}

func (controller *SessionController) withDesktop(ctx context.Context, session computer.ExecutionSession, action func(context.Context, computer.DesktopRuntime) error) error {
	return controller.withOperation(ctx, session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(graphicsAuthority)
		if !ok {
			return failure("agent_desktop_unavailable")
		}
		client, err := authority.desktopClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		if client == nil {
			return failure("agent_desktop_unavailable")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return action(ctx, client)
	})
}

func (controller *SessionController) StartDesktop(ctx context.Context, session computer.ExecutionSession) (status computer.DesktopStatus, failure error) {
	failure = controller.withDesktop(ctx, session, func(ctx context.Context, client computer.DesktopRuntime) (err error) {
		status, err = client.StartDesktop(ctx, session)
		return err
	})
	return status, failure
}
func (controller *SessionController) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (status computer.DesktopStatus, failure error) {
	failure = controller.withDesktop(ctx, session, func(ctx context.Context, client computer.DesktopRuntime) (err error) {
		status, err = client.InspectDesktop(ctx, session)
		return err
	})
	return status, failure
}

func (controller *SessionController) CaptureDesktop(ctx context.Context, request computer.DesktopCapture) (result computer.DesktopImage, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktop(ctx, request.Session, func(ctx context.Context, client computer.DesktopRuntime) (err error) {
		result, err = client.CaptureDesktop(ctx, request)
		return err
	})
	return result, failure
}

func (controller *SessionController) ReadDesktopImage(ctx context.Context, request computer.DesktopImageRead) (result computer.DesktopImageChunk, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktop(ctx, request.Session, func(ctx context.Context, client computer.DesktopRuntime) (err error) {
		result, err = client.ReadDesktopImage(ctx, request)
		return err
	})
	return result, failure
}

func (controller *SessionController) InputDesktop(ctx context.Context, request computer.DesktopInput) (result computer.DesktopInputReceipt, failure error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	failure = controller.withDesktop(ctx, request.Session, func(ctx context.Context, client computer.DesktopRuntime) (err error) {
		result, err = client.InputDesktop(ctx, request)
		return err
	})
	return result, failure
}
