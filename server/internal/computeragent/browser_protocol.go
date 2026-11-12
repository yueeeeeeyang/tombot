package computeragent

import (
	"context"
	"errors"
	"os"

	"io.yueyang/tom/internal/agentchannel"
)

type browserResult struct {
	Observation *BrowserObservation `json:"observation,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// StartBrowser 只发送一次启动请求；节点保留原入口，未知结果通过原授权查询或关闭。
func (client *Client) StartBrowser(ctx context.Context, grant BrowserGrant, files []*os.File) (BrowserObservation, error) {
	if len(files) != 3 {
		return BrowserObservation{}, errors.New("共享浏览器启动缺少受限入口")
	}
	return client.browserCall(ctx, "browser_start", grant, files, "active")
}

func (client *Client) BrowserStatus(ctx context.Context, grant BrowserGrant) (BrowserObservation, error) {
	return client.browserCall(ctx, "browser_status", grant, nil, "")
}

func (client *Client) CloseBrowser(ctx context.Context, grant BrowserGrant) (BrowserObservation, error) {
	return client.browserCall(ctx, "browser_close", grant, nil, "closed")
}

func (client *Client) browserCall(ctx context.Context, method string, grant BrowserGrant, files []*os.File, phase string) (BrowserObservation, error) {
	if grant.Validate() != nil {
		return BrowserObservation{}, errors.New("共享浏览器控制请求无效")
	}
	var result browserResult
	if err := client.exchange(ctx, method, grant, files, &result); err != nil {
		return BrowserObservation{}, err
	}
	observed := result.Observation
	if result.Error != "" || observed == nil || observed.Grant != grant || observed.Phase != "active" && observed.Phase != "closed" || phase != "" && observed.Phase != phase {
		return BrowserObservation{}, errors.New("共享浏览器控制回执未确认，需要节点对账")
	}
	return *observed, nil
}

func (server *Server) serveBrowser(ctx context.Context, channel *agentchannel.Channel, request agentchannel.Message, files []*os.File) (failure error) {
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	var grant BrowserGrant
	var observed BrowserObservation
	err := agentchannel.Decode(request.Body, &grant)
	if err == nil && request.Method != "browser_start" && len(files) != 0 {
		err = errors.New("共享浏览器控制包含意外入口")
	}
	if err == nil {
		switch request.Method {
		case "browser_start":
			observed, err = server.browsers.Start(ctx, grant, files)
			files = nil
		case "browser_status":
			observed, err = server.browsers.Status(ctx, grant)
		case "browser_close":
			observed, err = server.browsers.Close(ctx, grant)
		default:
			err = errors.New("共享浏览器控制动作无效")
		}
	}
	result := browserResult{}
	if err == nil {
		result.Observation = &observed
	} else {
		result.Error = "browser_rejected"
	}
	encoded, encodeErr := agentchannel.Encode(result)
	if encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return errors.Join(err, channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil))
}
