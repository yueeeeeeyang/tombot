package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func terminalHTTPFixture(t *testing.T) (*Server, *httptest.Server, auth.Session, service.Terminal) {
	t.Helper()
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "终端测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	response := request(t, handler, "POST", "/api/v1/terminals", map[string]any{"title": "测试终端"}, headers)
	if response.Code != 201 {
		t.Fatal("无法创建终端", response.Code)
	}
	var terminal service.Terminal
	if err = json.Unmarshal(response.Body.Bytes(), &terminal); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	return server, host, session, terminal
}

func terminalSocketDial(t *testing.T, host *httptest.Server, session auth.Session, id, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(host.URL, "http")+"/api/v1/terminals/"+id+query, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + session.DeviceToken}}})
	if err != nil {
		t.Fatal("终端连接失败", err)
	}
	t.Cleanup(func() {
		if err := connection.CloseNow(); err != nil {
			t.Log("终端连接已经关闭")
		}
	})
	return connection
}

func readTerminalFrame(t *testing.T, connection *websocket.Conn, match func(map[string]any) bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, connection, &frame); err != nil {
			t.Fatal("终端帧读取失败", err)
		}
		if match(frame) {
			return frame
		}
	}
}

func writeTerminalFrame(t *testing.T, connection *websocket.Conn, frame map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, connection, frame); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalHTTPWebSocketReconnectResizeAndExit(t *testing.T) {
	t.Parallel()
	server, host, session, terminal := terminalHTTPFixture(t)
	connection := terminalSocketDial(t, host, session, terminal.ID, "")
	readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "ready" })
	writeTerminalFrame(t, connection, map[string]any{"type": "resize", "cols": 115, "rows": 36, "seq": 1})
	readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "ack" && frame["seq"] == float64(1) })
	writeTerminalFrame(t, connection, map[string]any{"type": "input", "data": "stty -echo; printf 'first%s' 'result'\r", "seq": 2})
	var output strings.Builder
	last := readTerminalFrame(t, connection, func(frame map[string]any) bool {
		if frame["type"] == "output" {
			output.WriteString(frame["data"].(string))
		}
		return strings.Contains(output.String(), "firstresult")
	})
	cursor := int64(last["cursor"].(float64))
	if err := connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	current, err := server.Terminals.Get(context.Background(), terminal.ID)
	if err != nil || current.Status != "running" || current.Columns != 115 {
		t.Fatal("断线改变会话状态或尺寸没有保存", err)
	}
	reconnected := terminalSocketDial(t, host, session, terminal.ID, "?after="+strconv.FormatInt(cursor, 10))
	readTerminalFrame(t, reconnected, func(frame map[string]any) bool { return frame["type"] == "ready" })
	writeTerminalFrame(t, reconnected, map[string]any{"type": "input", "data": "stty size; printf 'second%s' 'result'; exit 4\r", "seq": 3})
	output.Reset()
	exit := readTerminalFrame(t, reconnected, func(frame map[string]any) bool {
		if frame["type"] == "output" {
			output.WriteString(frame["data"].(string))
		}
		return frame["type"] == "exit"
	})
	if exit["code"] != float64(4) || strings.Contains(output.String(), "firstresult") || !strings.Contains(output.String(), "secondresult") || !strings.Contains(output.String(), "36 115") {
		t.Fatal("终端续传、尺寸或退出码不符合契约")
	}
	response := request(t, server.Handler(), "DELETE", "/api/v1/terminals/"+terminal.ID, nil, map[string]string{"Authorization": "Bearer " + session.DeviceToken})
	if response.Code != 200 {
		t.Fatal("终端关闭失败", response.Code)
	}
}

func TestTerminalReadOnlyAndRevokedDeviceCannotWrite(t *testing.T) {
	t.Parallel()
	server, host, session, terminal := terminalHTTPFixture(t)
	readonly := terminalSocketDial(t, host, session, terminal.ID, "?readonly=1")
	readTerminalFrame(t, readonly, func(frame map[string]any) bool { return frame["type"] == "ready" })
	writeTerminalFrame(t, readonly, map[string]any{"type": "input", "data": "touch denied\r", "seq": 1})
	frame := readTerminalFrame(t, readonly, func(frame map[string]any) bool { return frame["type"] == "error" })
	if frame["code"] != "TERMINAL_READONLY" {
		t.Fatal("只读连接接受输入")
	}
	writeTerminalFrame(t, readonly, map[string]any{"type": "resize", "cols": 80, "rows": 20, "seq": 2})
	frame = readTerminalFrame(t, readonly, func(frame map[string]any) bool { return frame["type"] == "error" })
	if frame["code"] != "TERMINAL_READONLY" {
		t.Fatal("只读连接接受尺寸修改")
	}
	connection := terminalSocketDial(t, host, session, terminal.ID, "")
	readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "ready" })
	if err := server.Auth.Revoke(context.Background(), session.DeviceID, session.DeviceID); err != nil {
		t.Fatal(err)
	}
	writeTerminalFrame(t, connection, map[string]any{"type": "input", "data": "touch denied\r", "seq": 3})
	frame = readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "error" })
	if frame["code"] != "AUTH_REQUIRED" {
		t.Fatal("已撤销设备仍可操作", frame["code"])
	}
	if err := server.Terminals.Close(context.Background(), terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalHTTPAuthenticationOriginAndFrameValidation(t *testing.T) {
	t.Parallel()
	server, host, session, terminal := terminalHTTPFixture(t)
	handler := server.Handler()
	for _, path := range []string{"/api/v1/terminals", "/api/v1/terminals/" + terminal.ID} {
		if response := request(t, handler, "GET", path, nil, nil); response.Code != 401 {
			t.Fatal("终端接口缺少认证")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(host.URL, "http")+"/api/v1/terminals/"+terminal.ID, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + session.DeviceToken}, "Origin": []string{"https://untrusted.example"}}})
	if err == nil {
		if closeErr := connection.CloseNow(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("跨源终端连接未拒绝")
	}
	if response == nil || response.StatusCode != 403 {
		t.Fatal("跨源拒绝状态错误")
	}
	connection = terminalSocketDial(t, host, session, terminal.ID, "")
	readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "ready" })
	for _, input := range []map[string]any{{"type": "unknown"}, {"type": "input", "data": "true\r", "extra": true}, {"type": "resize", "cols": 501, "rows": 20}, {"type": "input", "data": ""}} {
		writeTerminalFrame(t, connection, input)
		readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "error" })
	}
	writeTerminalFrame(t, connection, map[string]any{"type": "input", "data": "exit 0\r"})
	readTerminalFrame(t, connection, func(frame map[string]any) bool { return frame["type"] == "exit" })
}
