package nodeapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
)

type ServerOptions struct {
	NodeID                       string
	ClientUID, ClientGID         uint32
	CallTimeout, ShutdownTimeout time.Duration
	MaximumConnections           int
}

var ErrUnfinishedOperations = errors.New("节点在途动作未确认收尾，进程必须退出")

// Validate 检查节点身份、唯一控制账户与调用/退出时限；不替代监听时的内核对端认证。
func (options ServerOptions) Validate() error {
	if !validNode(options.NodeID) || options.ClientUID == 0 || options.ClientGID == 0 || options.ClientUID == ^uint32(0) || options.ClientGID == ^uint32(0) || !validTimeout(options.CallTimeout) || options.ShutdownTimeout < options.CallTimeout || options.ShutdownTimeout > 2*time.Minute || options.MaximumConnections < 1 || options.MaximumConnections > 1024 {
		return errors.New("电脑节点服务配置无效")
	}
	return nil
}

type Server struct {
	operations Operations
	options    ServerOptions
	logger     *slog.Logger
}

// NewServer 固定唯一非 root 控制身份及节点标识，缺少任一电脑能力实现时拒绝构造。
func NewServer(operations Operations, options ServerOptions, logger *slog.Logger) (*Server, error) {
	if operations.Node == nil || operations.Computers == nil || operations.Sessions == nil || operations.Terminals == nil || operations.Desktops == nil || operations.Agents == nil || options.Validate() != nil || logger == nil {
		return nil, errors.New("电脑节点服务配置无效")
	}
	return &Server{operations: operations, options: options, logger: logger}, nil
}

// Serve 只接受已受保护的顺序包监听器；连接数量有上限，取消关闭监听并等待在途动作收尾。
func (server *Server) Serve(ctx context.Context, listener *net.UnixListener) (failure error) {
	if listener == nil {
		return errors.New("电脑节点监听器缺失")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var closeOnce sync.Once
	var closeErr error
	closeListener := func() { closeOnce.Do(func() { closeErr = listener.Close() }) }
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { closeListener(); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
		closeListener()
		failure = errors.Join(failure, closeErr)
	}()
	var pending sync.WaitGroup
	capacity := make(chan struct{}, server.options.MaximumConnections)
	for ctx.Err() == nil {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() == nil {
				failure = errors.New("电脑节点监听失败")
			}
			break
		}
		select {
		case capacity <- struct{}{}:
			pending.Add(1)
			go func() {
				defer pending.Done()
				defer func() { <-capacity }()
				if err := server.serveConnection(ctx, connection); err != nil && ctx.Err() == nil {
					server.logger.WarnContext(ctx, "电脑节点请求未能完成", "node_id", server.options.NodeID)
				}
			}()
		default:
			if err := connection.Close(); err != nil {
				failure = errors.New("关闭超额节点连接失败")
			}
		}
		if failure != nil {
			break
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { pending.Wait(); close(done) }()
	timer := time.NewTimer(server.options.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return errors.Join(failure, ErrUnfinishedOperations)
	}
	return failure
}

func (server *Server) serveConnection(parent context.Context, connection *net.UnixConn) (failure error) {
	channel, peer, err := agentchannel.New(connection, server.options.CallTimeout)
	if err != nil {
		return errors.Join(err, connection.Close())
	}
	defer func() { failure = errors.Join(failure, channel.Close()) }()
	if peer.PID <= 1 || peer.UID != server.options.ClientUID || peer.GID != server.options.ClientGID {
		return errors.New("节点请求方不是配置的控制服务身份")
	}
	ctx, cancel := context.WithTimeout(parent, server.options.CallTimeout)
	defer cancel()
	message, files, err := channel.Receive(ctx)
	if err != nil {
		return err
	}
	if err := agentchannel.CloseFiles(files); err != nil {
		return err
	}
	if message.Method != "node_control" || message.Files != 0 {
		return errors.New("节点请求不接受其他协议或描述符")
	}
	var content request
	if agentchannel.Decode(message.Body, &content) != nil || content.NodeID != server.options.NodeID {
		return errors.New("节点请求内容或目标无效")
	}
	deadline, err := content.Deadline.Local(server.options.CallTimeout)
	if err != nil {
		return err
	}
	ctx, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	input, lifetime, err := decodeInput(server.options.NodeID, content.Action, content.Input)
	if err != nil {
		return err
	}
	if lifetime > 0 {
		if content.LeaseDeadline == nil {
			return errors.New("跨进程执行租约缺少绝对期限")
		}
		leaseDeadline, err := content.LeaseDeadline.Local(lifetime)
		if err != nil {
			return err
		}
		ctx = computer.WithLeaseDeadline(ctx, leaseDeadline)
	} else if content.LeaseDeadline != nil {
		return errors.New("当前节点动作不接受新的执行租约")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	output, callErr := server.operations.invoke(ctx, content.Action, input)
	result := response{NodeID: server.options.NodeID, Action: content.Action}
	if callErr == nil {
		callErr = ctx.Err()
	}
	if callErr == nil {
		callErr = validateResult(content.Action, input, output)
	}
	if callErr == nil {
		result.Result, callErr = agentchannel.Encode(output)
	}
	if callErr != nil {
		result.Result = nil
		result.Failure = &computer.Failure{Code: "node_operation_unknown"}
		var classified *computer.Failure
		if errors.As(callErr, &classified) && codePattern.MatchString(classified.Code) {
			result.Failure = &computer.Failure{Code: classified.Code, IsPermanent: classified.IsPermanent}
		}
		server.logger.WarnContext(ctx, "电脑节点动作未确认成功", "node_id", server.options.NodeID, "request_id", message.ID, "action", content.Action)
	}
	encoded, err := agentchannel.Encode(result)
	if err != nil {
		return err
	}
	return channel.Send(ctx, agentchannel.Message{Version: 1, ID: message.ID, Method: "result", Body: encoded}, nil)
}
