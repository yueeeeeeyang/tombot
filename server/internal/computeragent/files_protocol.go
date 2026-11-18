package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type fileResult struct {
	Receipt *computer.FileReceipt `json:"receipt,omitempty"`
	Error   string                `json:"error,omitempty"`
}

func (client *Client) TransferFile(ctx context.Context, request computer.FileRequest) (computer.FileReceipt, error) {
	if err := request.Validate(); err != nil {
		return computer.FileReceipt{}, err
	}
	var result fileResult
	if err := client.exchange(ctx, "file_transfer", request, nil, &result); err != nil {
		return computer.FileReceipt{}, err
	}
	if result.Error != "" || result.Receipt == nil || result.Receipt.Validate(request) != nil {
		return computer.FileReceipt{}, errors.New("电脑代理文件传输失败或响应无效")
	}
	return *result.Receipt, nil
}

func (server *Server) serveFileTransfer(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	var result fileResult
	var input computer.FileRequest
	failure := agentchannel.Decode(request.Body, &input)
	if files != 0 || input.Validate() != nil {
		failure = errors.New("电脑文件传输协议无效")
	}
	if failure == nil {
		var receipt computer.FileReceipt
		receipt, failure = server.controller.TransferFile(ctx, input)
		if failure == nil {
			failure = receipt.Validate(input)
		}
		result.Receipt = &receipt
	}
	if failure != nil {
		result = fileResult{Error: "file_transfer_rejected"}
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	return errors.Join(failure, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
