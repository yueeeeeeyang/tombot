package nodeapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/desktopstream"
)

func (router *Router) OpenDesktopStream(ctx context.Context, input computer.DesktopStream) (net.Conn, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	client, err := router.client(ctx, input.Session.NodeID)
	if err != nil {
		return nil, err
	}
	return client.OpenDesktopStream(ctx, input)
}

func (client *Client) OpenDesktopStream(ctx context.Context, input computer.DesktopStream) (net.Conn, error) {
	if input.Validate() != nil || input.Session.NodeID != client.options.NodeID {
		return nil, errors.New("节点桌面流身份无效")
	}
	encoded, err := agentchannel.Encode(input)
	if err != nil {
		return nil, err
	}
	connection, _, err := websocket.Dial(ctx, strings.TrimSuffix(client.options.URL, "/")+"/v2/desktop", &websocket.DialOptions{HTTPClient: client.http, HTTPHeader: http.Header{"X-Tom-Desktop": {base64.RawURLEncoding.EncodeToString(encoded)}}})
	if err != nil {
		return nil, errors.New("连接受信桌面流失败")
	}
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	connection.SetReadLimit(1 << 20)
	return stream, nil
}

func (server *Server) serveDesktopStream(w http.ResponseWriter, r *http.Request) {
	server.streams.Add(1)
	defer server.streams.Done()
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		writeFailure(w, 400)
		return
	}
	encoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Tom-Desktop"))
	var request computer.DesktopStream
	if err != nil || len(encoded) > 4096 || agentchannel.Decode(encoded, &request) != nil || request.Validate() != nil || request.Session.NodeID != server.options.NodeID {
		writeFailure(w, 400)
		return
	}
	runtime, ok := server.operations.Console.(computer.DesktopStreamRuntime)
	if !ok {
		writeFailure(w, 503)
		return
	}
	bounded, cancel := context.WithTimeout(r.Context(), server.options.CallTimeout)
	upstream, err := runtime.OpenDesktopStream(bounded, request)
	cancel()
	if err != nil {
		writeFailure(w, 503)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		if upstream.Close() != nil {
			server.logger.Warn("关闭桌面流失败", "bot_id", request.Session.BotID)
		}
		return
	}
	connection.SetReadLimit(1 << 20)
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Minute)
	defer stop()
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	connection.SetReadLimit(1 << 20)
	if err := desktopstream.Pipe(ctx, stream, upstream); err != nil && ctx.Err() == nil {
		server.logger.WarnContext(ctx, "桌面流连接结束", "bot_id", request.Session.BotID, "session_id", request.Session.SessionID)
	}
}
