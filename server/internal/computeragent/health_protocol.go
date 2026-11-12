package computeragent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/agentchannel"
)

type controlHealth struct {
	Identity Identity `json:"identity"`
	Scope    string   `json:"scope"`
	Version  int      `json:"version"`
}

type healthResult struct {
	Observation *controlHealth `json:"observation,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// CheckControl 只确认原实例的控制服务可以接单，不探测业务能力、不启动服务，也不续约任何 Bot。
func (client *Client) CheckControl(ctx context.Context, identity Identity) error {
	if identity.validate() != nil {
		return errors.New("电脑代理控制检查身份无效")
	}
	var result healthResult
	if err := client.exchange(ctx, "health", identity, nil, &result); err != nil {
		return err
	}
	if result.Error != "" || result.Observation == nil || *result.Observation != (controlHealth{Identity: identity, Scope: "agent_control", Version: ConfigurationVersion}) {
		return errors.New("电脑代理控制入口未确认")
	}
	return ctx.Err()
}

func (server *Server) serveHealth(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, fileCount int) error {
	var identity Identity
	err := agentchannel.Decode(request.Body, &identity)
	if err == nil && (fileCount != 0 || identity != server.controller.identity) {
		err = errors.New("电脑代理控制检查身份或入口无效")
	}
	result := healthResult{}
	if err == nil {
		bounded, finish, beginErr := server.controller.begin(ctx)
		err = beginErr
		if beginErr == nil {
			ctx = bounded
			defer finish()
			result.Observation = &controlHealth{Identity: identity, Scope: "agent_control", Version: ConfigurationVersion}
		}
	}
	if err != nil {
		result.Error = "control_unavailable"
	}
	encoded, encodeErr := agentchannel.Encode(result)
	if encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return errors.Join(err, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
