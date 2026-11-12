package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type gatewayBrowser struct {
	session computer.ExecutionSession
	window  Window
	calls   atomic.Int64
	failure error
}

func (browser *gatewayBrowser) own(address Address) error {
	if address.Session != browser.session || address.WindowID != browser.window.ID {
		return ErrDenied
	}
	browser.calls.Add(1)
	return browser.failure
}
func (browser *gatewayBrowser) Open(_ context.Context, address Address) (Window, error) {
	return browser.window, browser.own(address)
}
func (browser *gatewayBrowser) List(_ context.Context, session computer.ExecutionSession) ([]Window, error) {
	err := browser.own(Address{Session: session, WindowID: browser.window.ID})
	return []Window{browser.window}, err
}
func (browser *gatewayBrowser) Execute(_ context.Context, address Address, command Command) (Window, error) {
	if command.Kind != "type" || command.Text != "中文输入" || command.Sequence != 7 {
		return Window{}, ErrInvalid
	}
	return browser.window, browser.own(address)
}
func (browser *gatewayBrowser) Screenshot(_ context.Context, address Address) ([]byte, error) {
	return []byte("fixture-png"), browser.own(address)
}
func (browser *gatewayBrowser) RespondDialog(_ context.Context, address Address, reply DialogReply) error {
	if reply.ID != 3 || !reply.Accept || reply.Text != "中文答复" {
		return ErrInvalid
	}
	return browser.own(address)
}
func (browser *gatewayBrowser) CloseWindow(_ context.Context, address Address) error {
	return browser.own(address)
}

func gatewayFixture(t *testing.T) (*Gateway, *gatewayBrowser, *authorizations) {
	t.Helper()
	options, access, sessions := testOptions()
	browser := &gatewayBrowser{session: sessions[0], window: Window{ID: ids.New(), BotID: sessions[0].BotID, DesktopID: sessions[0].DesktopID, State: "ready", Sequence: 7}}
	gateway, err := NewGateway(browser, sessions[0], func(ctx context.Context, session computer.ExecutionSession, _ Peer) error {
		return access.check(ctx, session)
	}, GatewayOptions{Width: 960, Height: 640, MaximumConnections: 4, CallTimeout: time.Second, ShutdownTimeout: time.Second}, options.Logger)
	if err != nil {
		t.Fatal(err)
	}
	return gateway, browser, access
}

func gatewayRequest(gateway *Gateway, method, path, body string, peer *Peer) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Host = "browser.local"
	request.Header.Set("Content-Type", "application/json")
	if peer != nil {
		request = request.WithContext(context.WithValue(request.Context(), peerKey{}, *peer))
	}
	response := httptest.NewRecorder()
	gateway.handle(response, request)
	return response
}

func TestGatewayRoutesUseBoundSession(t *testing.T) {
	gateway, browser, access := gatewayFixture(t)
	peer := &Peer{PID: 7, UID: uint32(browser.session.UID), GID: uint32(browser.session.GID)}
	base := "/v2/windows/" + browser.window.ID
	for _, test := range []struct{ method, path, body string }{
		{"GET", "/v2/state", ""}, {"PUT", base, "{}"}, {"POST", base + "/command", `{"sequence":7,"kind":"type","text":"中文输入"}`}, {"POST", base + "/dialog", `{"id":3,"accept":true,"text":"中文答复"}`}, {"GET", base + "/image", ""}, {"DELETE", base, ""},
	} {
		response := gatewayRequest(gateway, test.method, test.path, test.body, peer)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("浏览器桌面接口没有调用所属窗口", test.path, response.Code)
		}
		if strings.HasSuffix(test.path, "/image") && response.Header().Get("Content-Type") != "image/png" {
			t.Fatal("画面没有按图片返回")
		}
		if test.path == "/v2/state" {
			var state struct {
				Version, Width, Height int
				Windows                []Window
			}
			if json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Version != 2 || state.Width != 960 || len(state.Windows) != 1 || state.Windows[0].Sequence != 7 {
				t.Fatal("桌面状态缺少协议、视口或操作序号")
			}
		}
	}
	if browser.calls.Load() != 6 {
		t.Fatal("接口调用次数错误", browser.calls.Load())
	}
	access.revoke(browser.session)
	if response := gatewayRequest(gateway, "GET", "/v2/state", "", peer); response.Code != 403 || browser.calls.Load() != 6 {
		t.Fatal("撤销后仍调用浏览器")
	}
}

func TestGatewayRejectsIdentityInjectionAndMalformedRequests(t *testing.T) {
	gateway, browser, _ := gatewayFixture(t)
	peer := &Peer{PID: 7, UID: uint32(browser.session.UID), GID: uint32(browser.session.GID)}
	base := "/v2/windows/" + browser.window.ID
	for _, test := range []struct{ method, path, body string }{
		{"PUT", base, `{"user_id":"other"}`}, {"POST", base + "/command", `{"sequence":7,"kind":"type","text":"中文输入","session_id":"other"}`}, {"POST", base + "/command", `{}` + strings.Repeat(" ", 32768)}, {"GET", "/v2/state", "{}"}, {"GET", "/v2/state?user=other", ""}, {"PUT", "/v2/windows/invalid", "{}"},
	} {
		if response := gatewayRequest(gateway, test.method, test.path, test.body, peer); response.Code != 400 {
			t.Fatal("非法请求未拒绝", test.path, response.Code)
		}
	}
	if response := gatewayRequest(gateway, "GET", "/v2/state", "", nil); response.Code != 403 {
		t.Fatal("缺少内核身份仍可调用")
	}
	foreign := *peer
	foreign.UID++
	if response := gatewayRequest(gateway, "GET", "/v2/state", "", &foreign); response.Code != 403 {
		t.Fatal("跨 UID 调用没有拒绝")
	}
	request := httptest.NewRequest("GET", "/v2/state", nil)
	request.Host = "browser.local"
	request.Header.Set("Origin", "https://example.test")
	request = request.WithContext(context.WithValue(request.Context(), peerKey{}, *peer))
	response := httptest.NewRecorder()
	gateway.handle(response, request)
	if response.Code != 400 || browser.calls.Load() != 0 {
		t.Fatal("来源或身份注入仍触达浏览器")
	}
	if response := gatewayRequest(gateway, "GET", "/v2/windows/"+ids.New()+"/image", "", peer); response.Code != 403 {
		t.Fatal("窗口归属拒绝被丢弃")
	}
}

func TestGatewayErrorsDoNotExposeProtocolContents(t *testing.T) {
	gateway, browser, _ := gatewayFixture(t)
	peer := &Peer{PID: 7, UID: uint32(browser.session.UID), GID: uint32(browser.session.GID)}
	for failure, expected := range map[error]int{ErrUnknown: 409, ErrConflict: 409, ErrDialog: 409, ErrBusy: 409, ErrNavigation: 422, ErrCapacity: 429, errors.New("private protocol content"): 503} {
		browser.failure = failure
		response := gatewayRequest(gateway, "GET", "/v2/state", "", peer)
		if response.Code != expected || bytes.Contains(response.Body.Bytes(), []byte("private protocol content")) {
			t.Fatal("错误映射或内容保护无效", response.Code)
		}
	}
	gateway.mu.Lock()
	gateway.stopping = true
	gateway.mu.Unlock()
	if response := gatewayRequest(gateway, "GET", "/v2/state", "", peer); response.Code != 503 {
		t.Fatal("停服后仍接收窗口请求")
	}
}

func TestGatewayRejectsOldSynchronousProtocol(t *testing.T) {
	gateway, browser, _ := gatewayFixture(t)
	peer := &Peer{PID: 7, UID: uint32(browser.session.UID), GID: uint32(browser.session.GID)}
	for _, path := range []string{"/v1/state", "/v1/windows/" + browser.window.ID} {
		if response := gatewayRequest(gateway, "GET", path, "", peer); response.Code != 404 {
			t.Fatal("旧同步协议被继续接受", response.Code)
		}
	}
	if browser.calls.Load() != 0 {
		t.Fatal("旧协议触达了浏览器")
	}
}
