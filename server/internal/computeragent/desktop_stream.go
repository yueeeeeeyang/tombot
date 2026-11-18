package computeragent

import (
	"context"
	"errors"
	"net"
	"os"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type desktopStreamResult struct {
	Ready bool `json:"ready"`
}

func (client *Client) OpenDesktopStream(ctx context.Context, request computer.DesktopStream) (net.Conn, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	var result desktopStreamResult
	files, err := client.exchangeFiles(ctx, "desktop_connect", request, nil, &result, 1)
	if err != nil {
		return nil, errors.Join(err, agentchannel.CloseFiles(files))
	}
	if !result.Ready {
		return nil, errors.Join(errors.New("电脑观看通道未就绪"), agentchannel.CloseFiles(files))
	}
	connection, err := net.FileConn(files[0])
	closeError := agentchannel.CloseFiles(files)
	if err != nil || closeError != nil {
		if connection != nil {
			closeError = errors.Join(closeError, connection.Close())
		}
		return nil, errors.Join(err, closeError)
	}
	return connection, nil
}

func (server *Server) serveDesktopStream(ctx context.Context, channel *agentchannel.Channel, message agentchannel.Message, count int) (failure error) {
	var request computer.DesktopStream
	if count != 0 || agentchannel.Decode(message.Body, &request) != nil || request.Validate() != nil {
		return errors.New("桌面观看协议无效")
	}
	var file *os.File
	failure = server.controller.withScope(ctx, request.Session, func(ctx context.Context, guard *desktopGuard) error {
		streamer, ok := guard.scope.(interface {
			OpenDesktopStreamFile(context.Context, computer.DesktopStream) (*os.File, error)
		})
		if !ok {
			return errors.New("电脑未提供桌面观看通道")
		}
		var err error
		file, err = streamer.OpenDesktopStreamFile(ctx, request)
		return err
	})
	if file != nil {
		defer func() { failure = errors.Join(failure, file.Close()) }()
	}
	if failure != nil {
		return failure
	}
	encoded, err := agentchannel.Encode(desktopStreamResult{Ready: true})
	if err != nil {
		return err
	}
	return channel.Send(ctx, agentchannel.Message{Version: 1, ID: message.ID, Method: "result", Files: 1, Body: encoded}, []*os.File{file})
}
