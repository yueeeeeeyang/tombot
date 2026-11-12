package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"io.yueyang/tom/internal/auth"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func browserScreenFixture(t *testing.T) (*Server, *httptest.Server, auth.Session, service.BrowserTab) {
	t.Helper()
	s, code := fixture(t, true)
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(context.Background(), password, "浏览器画面测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	tab, err := s.Browser.Open(context.Background(), service.BrowserOpenInput{URL: "https://example.com", BotID: bot.ID}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.Handler())
	t.Cleanup(host.Close)
	return s, host, session, tab
}

func dialBrowserScreen(t *testing.T, host *httptest.Server, session auth.Session, id, query string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(host.URL, "http")+"/api/v1/browser/tabs/"+id+"/screen"+query, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + session.DeviceToken}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.CloseNow(); err != nil {
			t.Log("画面连接已关闭")
		}
	})
	return connection
}

func readBrowserScreen(t *testing.T, connection *websocket.Conn, kind string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, connection, &frame); err != nil {
			t.Fatal("画面帧读取失败", err)
		}
		if frame["type"] == kind {
			return frame
		}
		if frame["type"] == "error" {
			t.Fatal("意外的画面错误", frame["code"])
		}
	}
}

func sendBrowserScreen(t *testing.T, connection *websocket.Conn, frame map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, connection, frame); err != nil {
		t.Fatal(err)
	}
}

func startBrowserScreen(t *testing.T, connection *websocket.Conn, id string) map[string]any {
	t.Helper()
	readBrowserScreen(t, connection, "ready")
	sendBrowserScreen(t, connection, map[string]any{"type": "start", "tab_id": id, "max_fps": 5, "quality": 60})
	readBrowserScreen(t, connection, "tab_changed")
	return readBrowserScreen(t, connection, "frame")
}

func TestBrowserScreenFramesInputVersionsAndReconnect(t *testing.T) {
	t.Parallel()
	s, host, session, tab := browserScreenFixture(t)
	connection := dialBrowserScreen(t, host, session, tab.ID, "")
	frame := startBrowserScreen(t, connection, tab.ID)
	meta := frame["meta"].(map[string]any)
	image, err := base64.StdEncoding.DecodeString(frame["data"].(string))
	if err != nil || len(image) == 0 || meta["w"] != float64(1280) || meta["h"] != float64(720) {
		t.Fatal("画面契约错误", err)
	}
	command := map[string]any{"type": "click", "x": 10, "y": 20, "seq": 1, "revision": tab.Revision, "page_revision": tab.PageRevision}
	sendBrowserScreen(t, connection, command)
	ack := readBrowserScreen(t, connection, "ack")
	updated := ack["tab"].(map[string]any)
	if ack["seq"] != float64(1) || updated["page_revision"].(float64) <= float64(tab.PageRevision) {
		t.Fatal("输入未确认或未更新页面版本")
	}
	sendBrowserScreen(t, connection, command)
	if failed := readBrowserScreen(t, connection, "error"); failed["code"] != "BROWSER_INPUT_SEQUENCE" {
		t.Fatal("重复输入未拒绝", failed["code"])
	}
	command["seq"] = 2
	sendBrowserScreen(t, connection, command)
	if failed := readBrowserScreen(t, connection, "error"); failed["code"] != "BROWSER_TAB_CHANGED" {
		t.Fatal("过期画面仍可点击", failed["code"])
	}
	if err = connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	current, err := s.Browser.Get(context.Background(), tab.ID)
	if err != nil || current.Status != "active" || current.Controller != "user" {
		t.Fatal("断线改变标签", err)
	}
	reconnected := dialBrowserScreen(t, host, session, tab.ID, "")
	startBrowserScreen(t, reconnected, tab.ID)
	sendBrowserScreen(t, reconnected, map[string]any{"type": "input", "text": "用户输入", "seq": 1, "revision": current.Revision, "page_revision": current.PageRevision})
	ack = readBrowserScreen(t, reconnected, "ack")
	if ack["tab"].(map[string]any)["page_revision"] != float64(tab.PageRevision+2) {
		t.Fatal("断线重放了输入或拒绝动作仍产生副作用")
	}
}

func TestBrowserScreenReadOnlyControlAndRevokedDevice(t *testing.T) {
	t.Parallel()
	s, host, session, tab := browserScreenFixture(t)
	readonly := dialBrowserScreen(t, host, session, tab.ID, "?readonly=1")
	startBrowserScreen(t, readonly, tab.ID)
	command := map[string]any{"type": "key", "key": "Enter", "seq": 1, "revision": tab.Revision, "page_revision": tab.PageRevision}
	sendBrowserScreen(t, readonly, command)
	if frame := readBrowserScreen(t, readonly, "error"); frame["code"] != "BROWSER_READONLY" {
		t.Fatal("只读连接允许输入")
	}
	tab, err := s.Browser.SetController(context.Background(), tab.ID, "bot", tab.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	connection := dialBrowserScreen(t, host, session, tab.ID, "")
	startBrowserScreen(t, connection, tab.ID)
	command["revision"] = tab.Revision
	sendBrowserScreen(t, connection, command)
	if frame := readBrowserScreen(t, connection, "error"); frame["code"] != "BROWSER_CONTROL_REQUIRED" {
		t.Fatal("未接管即可输入", frame["code"])
	}
	if err = s.Auth.Revoke(context.Background(), session.DeviceID, session.DeviceID); err != nil {
		t.Fatal(err)
	}
	if frame := readBrowserScreen(t, connection, "error"); frame["code"] != "AUTH_REQUIRED" {
		t.Fatal("设备撤销后仍继续画面流")
	}
}

func TestBrowserScreenOriginCapacityAndDisconnectCleanup(t *testing.T) {
	t.Parallel()
	s, host, session, tab := browserScreenFixture(t)
	address := "ws" + strings.TrimPrefix(host.URL, "http") + "/api/v1/browser/tabs/" + tab.ID + "/screen"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": []string{"Bearer " + session.DeviceToken}, "Origin": []string{"https://untrusted.example"}}
	connection, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		if closeErr := connection.CloseNow(); closeErr != nil {
			t.Log(closeErr)
		}
		t.Fatal("允许跨来源画面连接")
	}
	if response == nil || response.StatusCode != 403 {
		t.Fatal("跨来源拒绝状态错误")
	}
	connections := []*websocket.Conn{}
	for i := 0; i < maximumBrowserConnections; i++ {
		connection := dialBrowserScreen(t, host, session, tab.ID, "")
		readBrowserScreen(t, connection, "ready")
		connections = append(connections, connection)
	}
	headers.Del("Origin")
	connection, response, err = websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: headers})
	if err == nil {
		if closeErr := connection.CloseNow(); closeErr != nil {
			t.Log(closeErr)
		}
		t.Fatal("画面连接数量无界")
	}
	if response == nil || response.StatusCode != 429 {
		t.Fatal("连接容量未正确拒绝")
	}
	for _, connection := range connections {
		if err := connection.CloseNow(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.browserConnections.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("断线未释放连接容量")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBrowserScreenInputValidation(t *testing.T) {
	valid := []string{`{"type":"start"}`, `{"type":"start","max_fps":1,"quality":80}`, `{"type":"click","x":0,"y":0,"seq":1,"revision":1,"page_revision":1}`, `{"type":"input","text":"中文","seq":1,"revision":1,"page_revision":1}`}
	for _, payload := range valid {
		if _, err := parseBrowserScreenInput([]byte(payload)); err != nil {
			t.Fatal("合法帧未通过", err)
		}
	}
	invalid := []string{`null`, `{} `, `{"type":"start","max_fps":0}`, `{"type":"start","max_fps":6}`, `{"type":"start","quality":81}`, `{"type":"start","x":0}`, `{"type":"start"}{}`, `{"type":"click","x":1280,"y":0,"seq":1,"revision":1,"page_revision":1}`, `{"type":"click","x":0,"y":null,"seq":1,"revision":1,"page_revision":1}`, `{"type":"input","text":"test","seq":1}`, `{"type":"input","text":"","seq":1,"revision":1,"page_revision":1}`, `{"type":"key","key":"Unknown","seq":1,"revision":1,"page_revision":1}`, `{"type":"input","text":"test","seq":9007199254740993,"revision":1,"page_revision":1}`}
	for _, payload := range invalid {
		if _, err := parseBrowserScreenInput([]byte(payload)); err == nil {
			t.Fatal("非法帧被接受", payload)
		}
	}
}
