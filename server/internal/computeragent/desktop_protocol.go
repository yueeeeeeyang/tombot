package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type desktopRequest struct {
	Session computer.ExecutionSession `json:"session"`
}
type desktopResult struct {
	Status *computer.DesktopStatus `json:"status,omitempty"`
	Error  string                  `json:"error,omitempty"`
}

func (client *Client) desktopCall(ctx context.Context, method string, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	if err := session.Validate(); err != nil {
		return computer.DesktopStatus{}, err
	}
	var result desktopResult
	if err := client.exchange(ctx, method, desktopRequest{Session: session}, nil, &result); err != nil {
		return computer.DesktopStatus{}, err
	}
	if result.Error != "" {
		return computer.DesktopStatus{}, errors.New("电脑代理拒绝图形会话请求，状态需要对账")
	}
	if result.Status == nil || result.Status.Validate() != nil || result.Status.Session != session || method == "desktop_start" && result.Status.State == "absent" {
		return computer.DesktopStatus{}, errors.New("电脑代理图形会话响应无效或归属不符")
	}
	return *result.Status, nil
}

func (client *Client) StartDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return client.desktopCall(ctx, "desktop_start", session)
}
func (client *Client) InspectDesktop(ctx context.Context, session computer.ExecutionSession) (computer.DesktopStatus, error) {
	return client.desktopCall(ctx, "desktop_status", session)
}

func (server *Server) serveDesktop(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files int) error {
	var input desktopRequest
	err := agentchannel.Decode(request.Body, &input)
	if files != 0 {
		err = errors.New("图形会话请求不接受外部描述符")
	}
	var status computer.DesktopStatus
	if err == nil {
		switch request.Method {
		case "desktop_start":
			status, err = server.controller.StartDesktop(ctx, input.Session)
		case "desktop_status":
			status, err = server.controller.InspectDesktop(ctx, input.Session)
		default:
			err = errors.New("图形会话动作无效")
		}
	}
	result := desktopResult{}
	if err != nil {
		result.Error = "desktop_rejected"
	} else {
		result.Status = &status
	}
	encoded, encodeError := agentchannel.Encode(result)
	if encodeError != nil {
		return encodeError
	}
	return errors.Join(err, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
