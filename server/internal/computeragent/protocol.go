package computeragent

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

type Grant struct {
	Command  computer.SessionCommand `json:"command"`
	Group    processgroup.Reference  `json:"group"`
	Deadline agentchannel.Deadline   `json:"deadline"`
}

type Renewal struct {
	Command  computer.SessionCommand `json:"command"`
	Deadline agentchannel.Deadline   `json:"deadline"`
}

type controlResult struct {
	Observation *computer.SessionObservation `json:"observation,omitempty"`
	Error       string                       `json:"error,omitempty"`
}

// PeerCheck 必须根据内核身份验证对端。节点还须核实 PID 所属的容器，不能仅比较 root UID。
type PeerCheck func(agentchannel.Peer) error

// RequireHostPeer 用于私有 PID 命名空间内的代理；宿主控制进程在该空间中没有可见 PID。
func RequireHostPeer(peer agentchannel.Peer) error {
	if peer.PID != 0 || peer.UID != 0 || peer.GID != 0 {
		return errors.New("电脑代理控制对端不是受信宿主进程")
	}
	return nil
}

// Client 每次动作建立独立连接，不重发未知动作。Dial 只能连接已验证实例的受保护本机套接字。
type Client struct {
	dial    func(context.Context) (*net.UnixConn, error)
	check   PeerCheck
	timeout time.Duration
}

// ConnectionError 表示尚未建立连接、尚未发送控制动作；错误文本不包含底层路径或诊断内容。
type ConnectionError struct{ cause error }

func (*ConnectionError) Error() string         { return "电脑代理连接失败" }
func (failure *ConnectionError) Unwrap() error { return failure.cause }

func NewClient(dial func(context.Context) (*net.UnixConn, error), check PeerCheck, timeout time.Duration) (*Client, error) {
	if dial == nil || check == nil || timeout < time.Second || timeout > time.Minute {
		return nil, errors.New("电脑代理控制客户端配置无效")
	}
	return &Client{dial: dial, check: check, timeout: timeout}, nil
}

// Open 只传递四个当前会话入口，调用方继续拥有原始描述符并负责关闭。
func (client *Client) Open(ctx context.Context, grant Grant, files []*os.File) (computer.SessionObservation, error) {
	if grant.Command.Validate() != nil || grant.Group.Validate() != nil || grant.Group.Session != grant.Command.Session || len(files) != 4 || grant.Deadline.ExpiresNS <= 0 || grant.Deadline.TimeNamespace == 0 {
		return computer.SessionObservation{}, errors.New("电脑代理开启请求无效")
	}
	return client.call(ctx, "open", grant, files, grant.Command, "active")
}
func (client *Client) Renew(ctx context.Context, renewal Renewal) (computer.SessionObservation, error) {
	if renewal.Command.Validate() != nil || renewal.Deadline.ExpiresNS <= 0 || renewal.Deadline.TimeNamespace == 0 {
		return computer.SessionObservation{}, errors.New("电脑代理续约请求无效")
	}
	return client.call(ctx, "renew", renewal, nil, renewal.Command, "active")
}
func (client *Client) Close(ctx context.Context, command computer.SessionCommand) (computer.SessionObservation, error) {
	if command.Validate() != nil {
		return computer.SessionObservation{}, errors.New("电脑代理关闭请求无效")
	}
	return client.call(ctx, "close", command, nil, command, "closed")
}

func (client *Client) call(ctx context.Context, method string, body any, files []*os.File, command computer.SessionCommand, phase string) (computer.SessionObservation, error) {
	var result controlResult
	if err := client.exchange(ctx, method, body, files, &result); err != nil {
		return computer.SessionObservation{}, err
	}
	if result.Error != "" {
		return computer.SessionObservation{}, errors.New("电脑代理拒绝控制请求，执行状态需要对账")
	}
	if result.Observation == nil || !result.Observation.Matches(command, phase) {
		return computer.SessionObservation{}, errors.New("电脑代理响应会话或状态不符")
	}
	return *result.Observation, nil
}

// exchange 每次只发送一次请求，未知结果由业务操作身份对账，不在传输层重放。
func (client *Client) exchange(ctx context.Context, method string, body any, files []*os.File, result any) (failure error) {
	encoded, err := agentchannel.Encode(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	connection, err := client.dial(ctx)
	if err != nil {
		return &ConnectionError{cause: err}
	}
	channel, peer, err := agentchannel.New(connection, client.timeout)
	if err != nil {
		return errors.Join(err, connection.Close())
	}
	defer func() { failure = errors.Join(failure, channel.Close()) }()
	if err = client.check(peer); err != nil {
		return errors.New("电脑代理对端实例验证失败")
	}
	request := agentchannel.Message{Version: 1, ID: ids.New(), Method: method, Files: len(files), Body: encoded}
	if err = channel.Send(ctx, request, files); err != nil {
		return err
	}
	response, received, err := channel.Receive(ctx)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(received)) }()
	if response.ID != request.ID || response.Method != "result" || response.Files != 0 {
		return errors.New("电脑代理响应身份或描述符无效")
	}
	return agentchannel.Decode(response.Body, result)
}
