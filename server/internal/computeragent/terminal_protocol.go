package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type terminalResult struct {
	Address computer.TerminalAddress  `json:"address"`
	Output  *computer.TerminalOutput  `json:"output,omitempty"`
	Receipt *computer.TerminalReceipt `json:"receipt,omitempty"`
	Resized bool                      `json:"resized,omitempty"`
	Error   string                    `json:"error,omitempty"`
}

func (client *Client) terminalCall(ctx context.Context, method string, input any, address computer.TerminalAddress) (terminalResult, error) {
	var result terminalResult
	if err := client.exchange(ctx, method, input, nil, &result); err != nil {
		return result, err
	}
	if result.Error != "" {
		return terminalResult{}, errors.New("电脑代理拒绝终端操作，结果需要对账")
	}
	if result.Address != address {
		return terminalResult{}, errors.New("电脑代理终端响应归属不符")
	}
	return result, nil
}

func validateTerminalOutput(result terminalResult, cursor int64, maximum int) (computer.TerminalOutput, error) {
	output := result.Output
	if output == nil || result.Receipt != nil || result.Resized || output.Validate(cursor, maximum) != nil {
		return computer.TerminalOutput{}, errors.New("电脑代理终端输出无效")
	}
	return *output, nil
}

func (client *Client) StartTerminal(ctx context.Context, request computer.TerminalStart) (computer.TerminalOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalOutput{}, err
	}
	result, err := client.terminalCall(ctx, "start", request, request.TerminalAddress)
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	return validateTerminalOutput(result, 0, computer.MaximumTerminalReadBytes)
}

func (client *Client) ReadTerminal(ctx context.Context, request computer.TerminalRead) (computer.TerminalOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalOutput{}, err
	}
	result, err := client.terminalCall(ctx, "read", request, request.TerminalAddress)
	if err != nil {
		return computer.TerminalOutput{}, err
	}
	return validateTerminalOutput(result, request.Cursor, request.MaximumBytes)
}

func (client *Client) WriteTerminal(ctx context.Context, request computer.TerminalWrite) (computer.TerminalReceipt, error) {
	if err := request.Validate(); err != nil {
		return computer.TerminalReceipt{}, err
	}
	result, err := client.terminalCall(ctx, "write", request, request.TerminalAddress)
	if err != nil {
		return computer.TerminalReceipt{}, err
	}
	receipt := result.Receipt
	if result.Output != nil || result.Resized || receipt == nil || receipt.Validate(request) != nil {
		return computer.TerminalReceipt{}, errors.New("电脑代理终端输入回执无效")
	}
	return *receipt, nil
}

func (client *Client) ResizeTerminal(ctx context.Context, request computer.TerminalResize) error {
	if err := request.Validate(); err != nil {
		return err
	}
	result, err := client.terminalCall(ctx, "resize", request, request.TerminalAddress)
	if err != nil {
		return err
	}
	if !result.Resized || result.Output != nil || result.Receipt != nil {
		return errors.New("电脑代理终端尺寸回执无效")
	}
	return nil
}

func (server *Server) serveTerminal(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	result := terminalResult{}
	var failure error
	if files != 0 {
		failure = errors.New("终端协议不接受外部描述符")
	} else {
		switch request.Method {
		case "start":
			var input computer.TerminalStart
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.TerminalOutput
				output, failure = server.controller.StartTerminal(ctx, input)
				result.Address, result.Output = input.TerminalAddress, &output
			}
		case "read":
			var input computer.TerminalRead
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.TerminalOutput
				output, failure = server.controller.ReadTerminal(ctx, input)
				result.Address, result.Output = input.TerminalAddress, &output
			}
		case "write":
			var input computer.TerminalWrite
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var receipt computer.TerminalReceipt
				receipt, failure = server.controller.WriteTerminal(ctx, input)
				result.Address, result.Receipt = input.TerminalAddress, &receipt
			}
		case "resize":
			var input computer.TerminalResize
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				failure = server.controller.ResizeTerminal(ctx, input)
				result.Address, result.Resized = input.TerminalAddress, true
			}
		default:
			failure = errors.New("终端操作无效")
		}
	}
	if failure != nil {
		result = terminalResult{Error: "terminal_rejected"}
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	err = channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil)
	return errors.Join(failure, err)
}
