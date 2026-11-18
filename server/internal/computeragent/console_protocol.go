package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type consoleResult struct {
	Receipt *computer.ConsoleReceipt `json:"receipt,omitempty"`
	Error   string                   `json:"error,omitempty"`
}

func (client *Client) Console(ctx context.Context, request computer.ConsoleRequest) (computer.ConsoleReceipt, error) {
	if err := request.Validate(); err != nil {
		return computer.ConsoleReceipt{}, err
	}
	var result consoleResult
	if err := client.exchange(ctx, "console", request, nil, &result); err != nil {
		return computer.ConsoleReceipt{}, err
	}
	if result.Error != "" || result.Receipt == nil || result.Receipt.Validate(request) != nil {
		return computer.ConsoleReceipt{}, errors.New("电脑代理接管失败或回执无效")
	}
	return *result.Receipt, nil
}

func (server *Server) serveConsole(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	var input computer.ConsoleRequest
	var result consoleResult
	failure := agentchannel.Decode(request.Body, &input)
	if files != 0 || input.Validate() != nil {
		failure = errors.New("桌面接管协议无效")
	}
	if failure == nil {
		var receipt computer.ConsoleReceipt
		receipt, failure = server.controller.Console(ctx, input)
		if failure == nil {
			failure = receipt.Validate(input)
		}
		result.Receipt = &receipt
	}
	if failure != nil {
		result = consoleResult{Error: "console_rejected"}
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	return errors.Join(failure, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
