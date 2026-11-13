package nodeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

// ClientOptions 描述跨主机节点端点；URL 只接受明确的 HTTPS 来源，凭据来自客户端证书。
type ClientOptions struct {
	NodeID      string
	URL         string
	CallTimeout time.Duration
	TLS         *tls.Config
}

type Client struct {
	options ClientOptions
	http    *http.Client
}

// NewClient 构造受指纹固定的 HTTPS 客户端。
// 保留连接复用：终端与 Agent 读取是高频短请求，每次重新握手会把往返延迟放大成可用性问题。
func NewClient(options ClientOptions) (*Client, error) {
	if !validNode(options.NodeID) || !validTimeout(options.CallTimeout) || options.TLS == nil || len(options.TLS.Certificates) == 0 || options.TLS.MinVersion < tls.VersionTLS13 {
		return nil, errors.New("节点客户端配置无效")
	}
	endpoint, err := url.Parse(options.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("节点客户端地址必须是明确的 HTTPS 来源")
	}
	transport := &http.Transport{
		TLSClientConfig:     options.TLS,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{options: options, http: &http.Client{Transport: transport, Timeout: options.CallTimeout}}, nil
}

// CloseIdle 释放空闲连接；不等待仍在进行的调用。
func (client *Client) CloseIdle() error {
	if transport, ok := client.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}

// exchange 只发送一次动作。
// 传输失败、超时、取消或响应无法校验都必须按结果未知处理：网络不等于执行失败，
// 上层据此对账而不是补发。
func (client *Client) exchange(ctx context.Context, action string, input, target any) (failure error) {
	encoded, err := agentchannel.Encode(input)
	if err != nil {
		return err
	}
	_, lifetime, err := decodeInput(client.options.NodeID, action, encoded)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, client.options.CallTimeout)
	defer cancel()
	requestID := ids.New()
	content := request{RequestID: requestID, NodeID: client.options.NodeID, Action: action, RemainingMillis: remaining(deadlineOf(callCtx), client.options.CallTimeout), Input: encoded}
	if lifetime > 0 {
		lease := remaining(computer.LeaseDeadline(ctx, lifetime), lifetime)
		if lease < 1 {
			return errors.New("执行租约在发送前已到期")
		}
		content.LeaseRemainMillis = &lease
	}
	body, err := agentchannel.Encode(content)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, strings.TrimSuffix(client.options.URL, "/")+"/v2/actions", bytes.NewReader(body))
	if err != nil {
		return errors.New("节点请求无法构造")
	}
	request.Header.Set("Content-Type", "application/json")
	reply, err := client.http.Do(request)
	if err != nil {
		return errors.Join(errors.New("连接受信电脑节点失败"), callCtx.Err())
	}
	defer func() { failure = errors.Join(failure, reply.Body.Close()) }()
	if reply.StatusCode != http.StatusOK {
		return errors.New("电脑节点拒绝了请求")
	}
	replyBody, err := io.ReadAll(io.LimitReader(reply.Body, agentchannel.MaximumBytes+1))
	if err != nil || len(replyBody) > agentchannel.MaximumBytes {
		return errors.New("电脑节点响应无法读取或超过限制")
	}
	var result response
	if agentchannel.Decode(replyBody, &result) != nil || result.RequestID != requestID || result.NodeID != client.options.NodeID || result.Action != action {
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

func deadlineOf(ctx context.Context) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Now()
	}
	return deadline
}

func validID(value string) bool {
	id, err := ulid.ParseStrict(value)
	return err == nil && id.String() == value
}
