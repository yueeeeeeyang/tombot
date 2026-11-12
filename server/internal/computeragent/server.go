package computeragent

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

// Server 处理会话授权与受管终端协议；完整电脑就绪仍要求文件、桌面与浏览器能力探测。
type Server struct {
	controller *Controller
	probes     *ProbeController
	browsers   *BrowserController
	check      PeerCheck
	timeout    time.Duration
	maximum    int
	logger     *slog.Logger
}

func NewServer(controller *Controller, probes *ProbeController, browsers *BrowserController, check PeerCheck, timeout time.Duration, maximum int, logger *slog.Logger) (*Server, error) {
	if controller == nil || probes == nil || browsers == nil || controller.identity != probes.identity || controller.identity != browsers.identity || check == nil || logger == nil || timeout < time.Second || timeout > time.Minute || maximum < 1 || maximum > 128 {
		return nil, errors.New("电脑代理控制服务配置无效")
	}
	return &Server{controller: controller, probes: probes, browsers: browsers, check: check, timeout: timeout, maximum: maximum, logger: logger}, nil
}

// Serve 接管已在 root 私有目录中建立的 Unix 顺序包监听器；取消后关闭接入并等待在途请求结束。
// 返回后入口仍必须调用 Controller.Shutdown；不能把停止监听等同于执行进程已经清理。
func (server *Server) Serve(ctx context.Context, listener *net.UnixListener) (failure error) {
	if listener == nil {
		return errors.New("电脑代理控制监听器缺失")
	}
	closeResult := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { closeResult <- listener.Close() })
	var pending sync.WaitGroup
	defer func() {
		if stop() {
			if listener.Close() != nil {
				failure = errors.Join(failure, errors.New("关闭电脑代理控制监听失败"))
			}
		} else if <-closeResult != nil {
			failure = errors.Join(failure, errors.New("取消电脑代理控制监听失败"))
		}
		pending.Wait()
	}()
	slots := make(chan struct{}, server.maximum)
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("电脑代理接受控制连接失败")
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return errors.Join(ctx.Err(), connection.Close())
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer func() { <-slots }()
			call, cancel := context.WithTimeout(ctx, server.timeout)
			defer cancel()
			if err := server.serveConnection(call, connection); err != nil && ctx.Err() == nil {
				server.logger.WarnContext(ctx, "电脑代理控制请求未确认", "user_id", server.controller.identity.UserID, "computer_id", server.controller.identity.ComputerID)
			}
		}()
	}
}

func (server *Server) serveConnection(ctx context.Context, connection *net.UnixConn) (failure error) {
	channel, peer, err := agentchannel.New(connection, server.timeout)
	if err != nil {
		return errors.Join(err, connection.Close())
	}
	defer func() { failure = errors.Join(failure, channel.Close()) }()
	if err = server.check(peer); err != nil {
		return errors.New("电脑代理拒绝未经验证的控制对端")
	}
	request, files, err := channel.Receive(ctx)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, agentchannel.CloseFiles(files)) }()
	if request.Method == "health" {
		return server.serveHealth(ctx, channel, request, len(files))
	}
	if request.Method == "browser_start" || request.Method == "browser_status" || request.Method == "browser_close" {
		owned := files
		files = nil
		return server.serveBrowser(ctx, channel, request, owned)
	}
	if request.Method == "probe" {
		owned := files
		files = nil
		return server.serveProbe(ctx, channel, request, owned)
	}
	if request.Method == "start" || request.Method == "read" || request.Method == "write" || request.Method == "resize" {
		return server.serveTerminal(ctx, channel, request, len(files))
	}
	if request.Method == "desktop_start" || request.Method == "desktop_status" {
		return server.serveDesktop(ctx, channel, request, len(files))
	}
	if request.Method == "desktop_capture" || request.Method == "desktop_image_read" || request.Method == "desktop_input" {
		return server.serveDesktopIO(ctx, channel, request, len(files))
	}
	if request.Method == "agent_start" || request.Method == "agent_write" || request.Method == "agent_read" {
		return server.serveAgent(ctx, channel, request, len(files))
	}
	var observation computer.SessionObservation
	switch request.Method {
	case "open":
		var input Grant
		err = agentchannel.Decode(request.Body, &input)
		if err == nil && len(files) != 4 {
			err = errors.New("电脑代理开启缺少受限描述符")
		}
		if err == nil {
			var deadline time.Time
			deadline, err = input.Deadline.Local(input.Command.Lifetime)
			if err == nil {
				observation, err = server.controller.openAt(ctx, input.Command, input.Group, files, deadline)
				files = nil
			}
		}
	case "renew":
		var input Renewal
		err = agentchannel.Decode(request.Body, &input)
		if err == nil && len(files) != 0 {
			err = errors.New("电脑代理控制动作包含意外描述符")
		}
		if err == nil {
			var deadline time.Time
			deadline, err = input.Deadline.Local(input.Command.Lifetime)
			if err == nil {
				observation, err = server.controller.renewAt(ctx, input.Command, deadline)
			}
		}
	case "close":
		var command computer.SessionCommand
		err = agentchannel.Decode(request.Body, &command)
		if err == nil && len(files) != 0 {
			err = errors.New("电脑代理控制动作包含意外描述符")
		}
		if err == nil {
			observation, err = server.controller.Close(ctx, command)
		}
	default:
		err = errors.New("电脑代理不支持此控制动作")
	}
	result := controlResult{}
	if err != nil {
		result.Error = "control_rejected"
	} else {
		result.Observation = &observation
	}
	encoded, encodeError := agentchannel.Encode(result)
	if encodeError != nil {
		return encodeError
	}
	if sendError := channel.Send(ctx, agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Files: 0, Body: encoded}, nil); sendError != nil {
		return sendError
	}
	return err
}
