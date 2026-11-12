package computeragent

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computerprobe"
)

type probeResult struct {
	Grant    ProbeGrant              `json:"grant"`
	Evidence *computerprobe.Evidence `json:"evidence,omitempty"`
	Closed   bool                    `json:"closed"`
	Error    string                  `json:"error,omitempty"`
}

// Probe 只发送一次固定检查请求，调用方保留原描述符；未知回包必须由节点回收原服务组。
func (client *Client) Probe(ctx context.Context, grant ProbeGrant, files []*os.File) (computerprobe.Evidence, error) {
	if grant.Validate() != nil || len(files) != 3 {
		return computerprobe.Evidence{}, errors.New("电脑代理探针请求无效")
	}
	var result probeResult
	if err := client.exchange(ctx, "probe", grant, files, &result); err != nil {
		return computerprobe.Evidence{}, err
	}
	if result.Grant != grant || result.Error != "" || !result.Closed || result.Evidence == nil || result.Evidence.Validate(grant.Request.Group.ID) != nil {
		return computerprobe.Evidence{}, errors.New("电脑代理探针证据或回收确认无效，需要节点对账")
	}
	return *result.Evidence, nil
}

func (server *Server) serveProbe(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files []*os.File) (failure error) {
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	var grant ProbeGrant
	result := probeResult{}
	err := agentchannel.Decode(request.Body, &grant)
	if err == nil {
		result.Grant = grant
		var evidence computerprobe.Evidence
		evidence, err = server.probes.Check(ctx, grant, files)
		files = nil
		if err == nil {
			result.Evidence, result.Closed = &evidence, true
		}
	}
	if err != nil {
		result.Error = "probe_rejected"
	}
	encoded, encodeErr := agentchannel.Encode(result)
	if encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return errors.Join(err, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
