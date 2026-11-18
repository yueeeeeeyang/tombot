package dockernode

import (
	"context"
	"errors"
	"net"

	"io.yueyang/tom/internal/computer"
)

func (controller *SessionController) OpenDesktopStream(ctx context.Context, request computer.DesktopStream) (connection net.Conn, err error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	err = controller.withOperation(ctx, request.Session, func(ctx context.Context, state *sessionRecord) error {
		authority, ok := controller.agent.(consoleAuthority)
		if !ok {
			return failure("agent_stream_unavailable")
		}
		client, err := authority.consoleClient(ctx, state.Command, state.Group)
		if err != nil {
			return err
		}
		streamer, ok := client.(computer.DesktopStreamRuntime)
		if !ok {
			return failure("agent_stream_unavailable")
		}
		connection, err = streamer.OpenDesktopStream(ctx, request)
		return err
	})
	if err != nil && connection != nil {
		err = errors.Join(err, connection.Close())
		connection = nil
	}
	return connection, err
}
