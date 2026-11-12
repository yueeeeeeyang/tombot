package sharedbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
)

// Client 是容器内 Agent 的本机入口，不接受网络地址、代理或任意 CDP 命令。
// 每次请求重新核验 Unix 对端；不重试动作、不跟随重定向、不复用连接。
type Client struct {
	session computer.ExecutionSession
	http    *http.Client
}

type State struct {
	Version int      `json:"version"`
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	Windows []Window `json:"windows"`
}

func NewClient(session computer.ExecutionSession) (*Client, error) {
	if session.Validate() != nil {
		return nil, ErrInvalid
	}
	transport := &http.Transport{DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 4096,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialLocalBrowser(ctx, session) },
	}
	return &Client{session: session, http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}}, nil
}

// request 的读取额度覆盖最大 32 个窗口及对话框；PNG 另受 4 MiB 上限约束。
func (client *Client) request(ctx context.Context, method, path string, input, output any) ([]byte, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil || len(encoded) > 32768 {
			return nil, ErrInvalid
		}
		body = bytes.NewReader(encoded)
	}
	return client.send(ctx, method, path, body, output)
}

func (client *Client) send(ctx context.Context, method, path string, body io.Reader, output any) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://browser.local"+path, body)
	if err != nil {
		return nil, ErrInvalid
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	limit := int64(8 << 20)
	if output == nil {
		limit = domain.MaxVisionImageBytes
	}
	encoded, readError := io.ReadAll(io.LimitReader(response.Body, limit+1))
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || int64(len(encoded)) > limit || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if len(encoded) > 4096 || decodeClientJSON(encoded, &failure) != nil {
			return nil, ErrUnavailable
		}
		for _, item := range []struct {
			status int
			code   string
			err    error
		}{
			{400, "invalid", ErrInvalid}, {403, "denied", ErrDenied}, {409, "conflict", ErrConflict}, {429, "capacity", ErrCapacity},
			{409, "unknown", ErrUnknown}, {409, "dialog", ErrDialog}, {409, "busy", ErrBusy}, {422, "navigation", ErrNavigation},
		} {
			if response.StatusCode == item.status && failure.Code == item.code {
				return nil, item.err
			}
		}
		return nil, ErrUnavailable
	}
	if output == nil {
		if response.Header.Get("Content-Type") != "image/png" {
			return nil, ErrUnavailable
		}
		return encoded, nil
	}
	if response.Header.Get("Content-Type") != "application/json; charset=utf-8" || decodeClientJSON(encoded, output) != nil {
		return nil, ErrUnavailable
	}
	return nil, nil
}

// Upload 传输 Agent 已读取的有界内容，不让 Chrome 根据用户路径重新打开文件。
func (client *Client) Upload(ctx context.Context, id string, request UploadRequest) (Window, error) {
	if err := request.Validate(); err != nil {
		return Window{}, err
	}
	path, err := client.windowPath(id)
	if err != nil {
		return Window{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maximumUploadBody {
		return Window{}, ErrInvalid
	}
	var window Window
	_, err = client.send(ctx, http.MethodPost, path+"/upload", bytes.NewReader(encoded), &window)
	if err == nil && (window.ID != id || window.Sequence != request.Sequence || client.validateWindow(window) != nil) {
		err = ErrUnavailable
	}
	return window, err
}

func decodeClientJSON(encoded []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return ErrUnavailable
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrUnavailable
	}
	return nil
}

func (client *Client) State(ctx context.Context) (State, error) {
	var state State
	_, err := client.request(ctx, http.MethodGet, "/v2/state", nil, &state)
	if err != nil {
		return State{}, err
	}
	if state.Version != 2 || state.Width < 320 || state.Width > 1280 || state.Height < 240 || state.Height > 800 || len(state.Windows) > 32 {
		return State{}, ErrUnavailable
	}
	seen := map[string]bool{}
	for _, window := range state.Windows {
		if client.validateWindow(window) != nil || seen[window.ID] {
			return State{}, ErrUnavailable
		}
		seen[window.ID] = true
	}
	sort.Slice(state.Windows, func(i, j int) bool { return state.Windows[i].ID < state.Windows[j].ID })
	return state, nil
}

func (client *Client) validateWindow(window Window) error {
	if _, err := ulid.ParseStrict(window.ID); err != nil {
		return ErrUnavailable
	}
	if window.ParentID != "" {
		if _, err := ulid.ParseStrict(window.ParentID); err != nil {
			return ErrUnavailable
		}
	}
	if window.BotID != client.session.BotID || window.DesktopID != client.session.DesktopID || window.Sequence < 0 || !validText(window.URL, 8192) || !validText(window.Title, 4096) || !validText(window.Notice, 4096) {
		return ErrUnavailable
	}
	switch window.State {
	case "ready", "unknown", "closing", "closed":
	default:
		return ErrUnavailable
	}
	switch window.CommandState {
	case "", "running", "waiting_dialog", "completed", "navigation_failed", "unknown":
	default:
		return ErrUnavailable
	}
	if window.Dialog.ID == 0 {
		if window.Dialog != (Dialog{}) {
			return ErrUnavailable
		}
	} else {
		if window.Dialog.ID < 0 || !validText(window.Dialog.Message, 16384) || !validText(window.Dialog.DefaultPrompt, 4096) {
			return ErrUnavailable
		}
		switch window.Dialog.Kind {
		case "alert", "confirm", "prompt", "beforeunload":
		default:
			return ErrUnavailable
		}
	}
	return nil
}

func (client *Client) windowPath(id string) (string, error) {
	if _, err := ulid.ParseStrict(id); err != nil {
		return "", ErrInvalid
	}
	return "/v2/windows/" + id, nil
}

func (client *Client) Open(ctx context.Context, id string) (Window, error) {
	path, err := client.windowPath(id)
	if err != nil {
		return Window{}, err
	}
	var window Window
	_, err = client.request(ctx, http.MethodPut, path, struct{}{}, &window)
	if err == nil && (window.ID != id || client.validateWindow(window) != nil) {
		err = ErrUnavailable
	}
	return window, err
}
func (client *Client) Execute(ctx context.Context, id string, command Command) (Window, error) {
	path, err := client.windowPath(id)
	if err != nil {
		return Window{}, err
	}
	var window Window
	_, err = client.request(ctx, http.MethodPost, path+"/command", command, &window)
	if err == nil && (window.ID != id || window.Sequence != command.Sequence || client.validateWindow(window) != nil) {
		err = ErrUnavailable
	}
	return window, err
}
func (client *Client) Screenshot(ctx context.Context, id string) ([]byte, error) {
	path, err := client.windowPath(id)
	if err != nil {
		return nil, err
	}
	return client.request(ctx, http.MethodGet, path+"/image", nil, nil)
}
func (client *Client) RespondDialog(ctx context.Context, id string, reply DialogReply) error {
	path, err := client.windowPath(id)
	if err != nil {
		return err
	}
	_, err = client.request(ctx, http.MethodPost, path+"/dialog", reply, &struct{}{})
	return err
}
func (client *Client) CloseWindow(ctx context.Context, id string) error {
	path, err := client.windowPath(id)
	if err != nil {
		return err
	}
	_, err = client.request(ctx, http.MethodDelete, path, nil, &struct{}{})
	return err
}
