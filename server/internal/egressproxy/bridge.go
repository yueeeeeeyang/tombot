package egressproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// BridgeOptions 是容器内环回转接的限额，真正的目标与 DNS 策略由宿主 Server 执行。
type BridgeOptions struct {
	ConnectTimeout, IdleTimeout, Lifetime, ShutdownTimeout time.Duration
	MaximumConnections                                     int
	MaximumTransferBytes                                   int64
}

func (options BridgeOptions) Validate() error {
	if options.ConnectTimeout < time.Second || options.ConnectTimeout > time.Minute || options.IdleTimeout < time.Second || options.IdleTimeout > time.Minute || options.Lifetime < options.IdleTimeout || options.Lifetime > time.Hour || options.Lifetime < options.ConnectTimeout || options.ShutdownTimeout < options.ConnectTimeout || options.ShutdownTimeout > 3*time.Minute || options.MaximumConnections < 1 || options.MaximumConnections > 256 || options.MaximumTransferBytes < 1024 || options.MaximumTransferBytes > 1<<40 {
		return errors.New("电脑出网转接限额无效")
	}
	return nil
}

type Bridge struct {
	options BridgeOptions
	dial    func(context.Context) (net.Conn, error)
	logger  *slog.Logger
}

// NewBridge 需要固定受保护 Unix 端点的连接器；不接受用户指定上游或任意代理 URL。
func NewBridge(options BridgeOptions, dial func(context.Context) (net.Conn, error), logger *slog.Logger) (*Bridge, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if dial == nil || logger == nil {
		return nil, errors.New("电脑出网转接依赖缺失")
	}
	return &Bridge{options, dial, logger}, nil
}

// Serve 只监听当前容器的 IPv4 环回地址。连接失败明确响应 502；不读取系统代理或自动重连。
func (bridge *Bridge) Serve(parent context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("电脑出网转接未配置监听器")
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return errors.New("电脑出网转接必须监听 IPv4 环回地址")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	closed := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		err := listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		closed <- err
	})
	var pending sync.WaitGroup
	slots := make(chan struct{}, bridge.options.MaximumConnections)
	var failure error
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				failure = errors.New("电脑出网转接监听提前结束")
			}
			break
		}
		select {
		case slots <- struct{}{}:
		default:
			if err := closeNetworkConnections(connection); err != nil {
				bridge.logger.WarnContext(ctx, "关闭超额出网转接失败")
			}
			continue
		}
		pending.Add(1)
		go func() { defer pending.Done(); defer func() { <-slots }(); bridge.forward(ctx, connection) }()
	}
	cancel()
	if stop() {
		failure = errors.Join(failure, listener.Close())
	} else {
		failure = errors.Join(failure, <-closed)
	}
	finished := make(chan struct{})
	go func() { pending.Wait(); close(finished) }()
	timer := time.NewTimer(bridge.options.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-finished:
		if failure != nil {
			return failure
		}
		return parent.Err()
	case <-timer.C:
		return errors.Join(failure, errors.New("电脑出网转接退出超时"))
	}
}

func (bridge *Bridge) forward(parent context.Context, client net.Conn) {
	defer func() {
		if err := closeNetworkConnections(client); err != nil {
			bridge.logger.WarnContext(parent, "关闭出网转接客户端失败")
		}
	}()
	ctx, cancel := context.WithTimeout(parent, bridge.options.Lifetime)
	defer cancel()
	dialContext, stop := context.WithTimeout(ctx, bridge.options.ConnectTimeout)
	upstream, err := bridge.dial(dialContext)
	stop()
	if err != nil {
		bridge.logger.WarnContext(parent, "电脑出网节点暂不可用")
		if deadlineErr := client.SetWriteDeadline(time.Now().Add(bridge.options.ConnectTimeout)); deadlineErr != nil {
			bridge.logger.WarnContext(parent, "设置出网失败响应时限失败")
			return
		}
		if _, writeErr := io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"); writeErr != nil {
			bridge.logger.WarnContext(parent, "出网失败响应未完成")
		}
		return
	}
	defer func() {
		if err := closeNetworkConnections(upstream); err != nil {
			bridge.logger.WarnContext(parent, "关闭出网转接上游失败")
		}
	}()
	deadline, _ := ctx.Deadline()
	left := &timedConnection{Conn: client, idle: bridge.options.IdleTimeout, deadline: deadline}
	right := &timedConnection{Conn: upstream, idle: bridge.options.IdleTimeout, deadline: deadline}
	if err := relay(ctx, left, left, right, bridge.options.MaximumTransferBytes); err != nil && parent.Err() == nil {
		bridge.logger.WarnContext(parent, "电脑出网转接未完成")
	}
}

// RequireHostConnection 供容器内转接器验证宿主 root 对端；宿主 PID 在容器私有 PID 空间应不可见。
func RequireHostConnection(connection *net.UnixConn) error {
	if connection == nil {
		return errors.New("电脑出网节点连接缺失")
	}
	peer, err := readPeer(connection)
	if err != nil || peer.UID != 0 || peer.GID != 0 || peer.PID != 0 {
		return errors.New("电脑出网上游不是宿主受信节点")
	}
	return nil
}
