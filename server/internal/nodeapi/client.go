package nodeapi

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type ClientOptions struct {
	NodeID      string
	Socket      string
	CallTimeout time.Duration
}

type Client struct {
	options ClientOptions
	dial    func(context.Context) (*net.UnixConn, error)
}

// NewClient 固定本机节点和受保护 socket；每次连接都核对内核 root 对端，不读取代理或 Docker 环境。
func NewClient(options ClientOptions) (*Client, error) {
	if !validNode(options.NodeID) || !validTimeout(options.CallTimeout) || !filepath.IsAbs(options.Socket) || filepath.Clean(options.Socket) != options.Socket || options.Socket == "/" || len(options.Socket) > 100 {
		return nil, errors.New("节点客户端配置无效")
	}
	return &Client{options: options, dial: func(ctx context.Context) (*net.UnixConn, error) { return dialControl(ctx, options.Socket) }}, nil
}

// exchange 不重发任何动作；节点调用期限和授权期限都使用同一内核的绝对单调时钟。
func (client *Client) exchange(ctx context.Context, action string, input, target any) (failure error) {
	encoded, err := agentchannel.Encode(input)
	if err != nil {
		return err
	}
	_, lifetime, err := decodeInput(client.options.NodeID, action, encoded)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, client.options.CallTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	callDeadline, _ := ctx.Deadline()
	deadline, err := agentchannel.FromDeadline(callDeadline)
	if err != nil {
		return err
	}
	content := request{NodeID: client.options.NodeID, Action: action, Deadline: deadline, Input: encoded}
	if lifetime > 0 {
		leaseDeadline, err := agentchannel.FromDeadline(computer.LeaseDeadline(ctx, lifetime))
		if err != nil {
			return err
		}
		content.LeaseDeadline = &leaseDeadline
	}
	body, err := agentchannel.Encode(content)
	if err != nil {
		return err
	}
	connection, err := client.dial(ctx)
	if err != nil {
		return errors.Join(errors.New("连接受信电脑节点失败"), ctx.Err())
	}
	channel, peer, err := agentchannel.New(connection, client.options.CallTimeout)
	if err != nil {
		return errors.Join(err, connection.Close())
	}
	defer func() { failure = errors.Join(failure, channel.Close()) }()
	if peer.UID != 0 || peer.PID <= 1 {
		return errors.New("电脑节点对端不是宿主受信进程")
	}
	message := agentchannel.Message{Version: 1, ID: ids.New(), Method: "node_control", Body: body}
	if err := channel.Send(ctx, message, nil); err != nil {
		return err
	}
	reply, files, err := channel.Receive(ctx)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	if reply.Method != "result" || reply.ID != message.ID || reply.Files != 0 {
		return errors.New("电脑节点响应身份或描述符无效")
	}
	var result response
	if agentchannel.Decode(reply.Body, &result) != nil || result.NodeID != client.options.NodeID || result.Action != action {
		return errors.New("电脑节点响应内容或归属无效")
	}
	if result.Failure != nil {
		if len(result.Result) != 0 || !codePattern.MatchString(result.Failure.Code) {
			return errors.New("电脑节点失败响应无效")
		}
		return result.Failure
	}
	if len(result.Result) == 0 || agentchannel.Decode(result.Result, target) != nil {
		return errors.New("电脑节点结果无效")
	}
	return nil
}
