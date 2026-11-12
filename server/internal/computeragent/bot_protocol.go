package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type agentStart struct {
	Session computer.ExecutionSession `json:"session"`
}
type agentResult struct {
	Session computer.ExecutionSession `json:"session"`
	Started bool                      `json:"started,omitempty"`
	Offset  *int64                    `json:"offset,omitempty"`
	Output  *computer.AgentOutput     `json:"output,omitempty"`
	Error   string                    `json:"error,omitempty"`
}

func (client *Client) agentCall(ctx context.Context, method string, input any, session computer.ExecutionSession) (agentResult, error) {
	var result agentResult
	if err := client.exchange(ctx, method, input, nil, &result); err != nil {
		return result, err
	}
	if result.Error != "" || result.Session != session {
		return agentResult{}, errors.New("Bot Agent 控制回包未确认或归属不符")
	}
	return result, nil
}

func (client *Client) StartAgent(ctx context.Context, session computer.ExecutionSession) error {
	if err := session.Validate(); err != nil {
		return err
	}
	result, err := client.agentCall(ctx, "agent_start", agentStart{Session: session}, session)
	if err != nil {
		return err
	}
	if !result.Started || result.Offset != nil || result.Output != nil {
		return errors.New("Bot Agent 启动回包无效")
	}
	return nil
}

func (client *Client) WriteAgent(ctx context.Context, request computer.AgentWrite) (int64, error) {
	if err := request.Validate(); err != nil {
		return 0, err
	}
	result, err := client.agentCall(ctx, "agent_write", request, request.Session)
	if err != nil {
		return 0, err
	}
	if result.Started || result.Output != nil || result.Offset == nil || *result.Offset != request.Offset+int64(len(request.Bytes)) {
		return 0, errors.New("Bot Agent 输入回执无效")
	}
	return *result.Offset, nil
}

func (client *Client) ReadAgent(ctx context.Context, request computer.AgentRead) (computer.AgentOutput, error) {
	if err := request.Validate(); err != nil {
		return computer.AgentOutput{}, err
	}
	result, err := client.agentCall(ctx, "agent_read", request, request.Session)
	if err != nil {
		return computer.AgentOutput{}, err
	}
	if result.Started || result.Offset != nil || result.Output == nil || result.Output.Validate(request.Offset) != nil {
		return computer.AgentOutput{}, errors.New("Bot Agent 输出回包无效")
	}
	return *result.Output, nil
}

func (server *Server) serveAgent(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	result := agentResult{}
	var failure error
	if files != 0 {
		failure = errors.New("Bot Agent 协议不接受外部描述符")
	} else {
		switch request.Method {
		case "agent_start":
			var input agentStart
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				failure = server.controller.StartAgent(ctx, input.Session)
				result.Session, result.Started = input.Session, true
			}
		case "agent_write":
			var input computer.AgentWrite
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var offset int64
				offset, failure = server.controller.WriteAgent(ctx, input)
				result.Session, result.Offset = input.Session, &offset
			}
		case "agent_read":
			var input computer.AgentRead
			failure = agentchannel.Decode(request.Body, &input)
			if failure == nil {
				var output computer.AgentOutput
				output, failure = server.controller.ReadAgent(ctx, input)
				result.Session, result.Output = input.Session, &output
			}
		default:
			failure = errors.New("Bot Agent 控制动作无效")
		}
	}
	if failure != nil {
		result = agentResult{Error: "agent_rejected"}
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	return errors.Join(failure, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
