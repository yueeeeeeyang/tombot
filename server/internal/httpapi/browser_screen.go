package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

const maximumBrowserConnections = 8
const browserSocketTimeout = 5 * time.Second
const browserScreenHeartbeat = 15 * time.Second
const maximumBrowserFPS = 5

type browserScreenInput struct {
	Type     string `json:"type"`
	TabID    string `json:"tab_id,omitempty"`
	Quality  int    `json:"quality,omitempty"`
	FPS      int    `json:"max_fps,omitempty"`
	Sequence int64  `json:"seq,omitempty"`
	service.BrowserVersion
	X      *float64 `json:"x,omitempty"`
	Y      *float64 `json:"y,omitempty"`
	Text   string   `json:"text,omitempty"`
	Key    string   `json:"key,omitempty"`
	DeltaX float64  `json:"delta_x,omitempty"`
	DeltaY float64  `json:"delta_y,omitempty"`
}

func (input browserScreenInput) command() service.BrowserAction {
	action := input.Type
	if action == "click" {
		action = "click_at"
	}
	return service.BrowserAction{BrowserVersion: input.BrowserVersion, Action: action, X: input.X, Y: input.Y, Text: input.Text, Key: input.Key, DeltaX: input.DeltaX, DeltaY: input.DeltaY}
}

// parseBrowserScreenInput 拒绝混用帧字段和空值；用户动作必须携带看到画面时的双版本。
func parseBrowserScreenInput(payload []byte) (browserScreenInput, error) {
	input := browserScreenInput{Quality: 60, FPS: maximumBrowserFPS}
	failure := func() (browserScreenInput, error) {
		return input, domain.NewError(400, "INVALID_BROWSER_FRAME", "Provide one supported browser JSON frame with valid fields.")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return failure()
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return failure()
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return failure()
	}
	allowed := map[string]bool{"type": true, "seq": true, "revision": true, "page_revision": true}
	switch input.Type {
	case "start":
		allowed = map[string]bool{"type": true, "tab_id": true, "quality": true, "max_fps": true}
	case "click":
		allowed["x"], allowed["y"] = true, true
	case "input":
		allowed["text"] = true
	case "key":
		allowed["key"] = true
	case "scroll":
		allowed["delta_x"], allowed["delta_y"] = true, true
	default:
		return failure()
	}
	for key, value := range fields {
		if !allowed[key] || string(value) == "null" {
			return failure()
		}
	}
	if input.Type == "start" {
		if input.Quality < 1 || input.Quality > 80 || input.FPS < 1 || input.FPS > maximumBrowserFPS {
			return failure()
		}
		return input, nil
	}
	if input.Sequence < 1 || input.Sequence > 1<<53 || input.Revision == 0 || input.PageRevision == 0 || input.Revision > 1<<53 || input.PageRevision > 1<<53 {
		return failure()
	}
	if input.Type == "input" && input.Text == "" {
		return failure()
	}
	if err := input.command().Validate(); err != nil {
		return input, err
	}
	return input, nil
}

type browserSocketFrame struct {
	body map[string]any
	stop bool
}

func browserScreenError(err error, sequence int64, stop bool) browserSocketFrame {
	var public *domain.Error
	if !errors.As(err, &public) {
		public = domain.NewError(500, "BROWSER_OPERATION_FAILED", "The browser operation failed. Inspect the page; do not replay input automatically.")
	}
	return browserSocketFrame{body: map[string]any{"type": "error", "code": public.Code, "message": public.Message, "seq": sequence}, stop: stop}
}

// browserScreenSocket 每连接只保留一个待发送画面；慢连接不会堆积帧，也不会阻塞浏览器命令锁。
// 画面读取可重试有限次，输入永不重发；连接断开不关闭标签或更改其控制权。
func (s *Server) browserScreenSocket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tab, err := s.Browser.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	readonly := r.URL.Query().Get("readonly")
	if readonly != "" && readonly != "0" && readonly != "1" {
		s.fail(w, r, domain.NewError(400, "INVALID_BROWSER_INPUT", "Readonly must be 0 or 1."))
		return
	}
	if tab.Status != "active" {
		s.fail(w, r, domain.NewError(409, "BROWSER_TAB_INACTIVE", "Restore the tab before opening its screen."))
		return
	}
	count := s.browserConnections.Add(1)
	defer s.browserConnections.Add(-1)
	if count > maximumBrowserConnections {
		s.fail(w, r, domain.NewError(429, "BROWSER_CONNECTION_LIMIT", "Too many browser screens are open."))
		return
	}
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.Logger.Debug("浏览器画面连接升级失败", "tab_id", id, "request_id", r.Context().Value(requestKey))
		return
	}
	connection.SetReadLimit(6*browser.MaxInputBytes + 1024)
	ctx, cancel := context.WithCancel(r.Context())
	stopRoot := context.AfterFunc(s.RootContext, cancel)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		stopRoot()
		if err := connection.CloseNow(); err != nil {
			s.Logger.Debug("浏览器画面连接已断开", "tab_id", id)
		}
		workers.Wait()
	}()
	write := func(frame map[string]any) error {
		operation, stop := context.WithTimeout(ctx, browserSocketTimeout)
		defer stop()
		return wsjson.Write(operation, connection, frame)
	}
	authenticate := func() error {
		operation, stop := context.WithTimeout(ctx, browserSocketTimeout)
		defer stop()
		_, err := s.Auth.Authenticate(operation, bearer(r))
		return err
	}
	if err = write(map[string]any{"type": "ready", "tab": tab, "readonly": readonly == "1"}); err != nil {
		return
	}
	handshake, stopHandshake := context.WithTimeout(ctx, 10*time.Second)
	kind, payload, err := connection.Read(handshake)
	stopHandshake()
	if err != nil {
		return
	}
	settings, err := parseBrowserScreenInput(payload)
	if kind != websocket.MessageText || settings.Type != "start" || settings.TabID != "" && settings.TabID != id {
		err = domain.NewError(400, "INVALID_BROWSER_FRAME", "Start the screen for this tab before sending input.")
	}
	if err != nil {
		if writeErr := write(browserScreenError(err, 0, true).body); writeErr != nil {
			s.Logger.Debug("浏览器画面握手响应中断", "tab_id", id)
		}
		return
	}
	if err = authenticate(); err != nil {
		if writeErr := write(browserScreenError(err, 0, true).body); writeErr != nil {
			s.Logger.Debug("浏览器画面鉴权响应中断", "tab_id", id)
		}
		return
	}
	if err = write(map[string]any{"type": "tab_changed", "tab_id": id}); err != nil {
		return
	}
	frames := make(chan browserSocketFrame, 1)
	replies := make(chan browserSocketFrame, 2)
	send := func(channel chan<- browserSocketFrame, frame browserSocketFrame) bool {
		select {
		case channel <- frame:
			return true
		case <-ctx.Done():
			return false
		}
	}
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer cancel()
		var sequence int64
		for {
			kind, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			input, parseErr := parseBrowserScreenInput(payload)
			if kind != websocket.MessageText || input.Type == "start" {
				parseErr = domain.NewError(400, "INVALID_BROWSER_FRAME", "Send a supported user action after starting the screen.")
			}
			if parseErr != nil {
				if !send(replies, browserScreenError(parseErr, input.Sequence, false)) {
					return
				}
				continue
			}
			if input.Sequence <= sequence {
				if !send(replies, browserScreenError(domain.NewError(409, "BROWSER_INPUT_SEQUENCE", "This input sequence has already been used. Never replay input."), input.Sequence, false)) {
					return
				}
				continue
			}
			sequence = input.Sequence
			if err = authenticate(); err != nil {
				if !send(replies, browserScreenError(err, input.Sequence, true)) {
					return
				}
				<-ctx.Done()
				return
			}
			if readonly == "1" {
				if !send(replies, browserScreenError(domain.NewError(403, "BROWSER_READONLY", "This screen connection is read-only."), input.Sequence, false)) {
					return
				}
				continue
			}
			updated, actionErr := s.Browser.Apply(ctx, id, input.command(), nil, actor(r))
			if actionErr != nil {
				var public *domain.Error
				if !errors.As(actionErr, &public) {
					s.Logger.Error("浏览器画面输入失败", "tab_id", id, "device_id", r.Context().Value(deviceKey))
				}
				if !send(replies, browserScreenError(actionErr, input.Sequence, false)) {
					return
				}
			} else if !send(replies, browserSocketFrame{body: map[string]any{"type": "ack", "seq": input.Sequence, "action": input.Type, "tab": updated}}) {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Second / time.Duration(settings.FPS))
		defer ticker.Stop()
		failures := 0
		for {
			frame, err := s.Browser.CaptureScreen(ctx, id, settings.Quality)
			if err != nil {
				var public *domain.Error
				failures++
				if errors.As(err, &public) && public.Code == "BROWSER_DOCUMENT_CHANGED" && failures <= 3 {
					if !send(frames, browserSocketFrame{body: map[string]any{"type": "loading"}}) {
						return
					}
				} else {
					send(frames, browserScreenError(err, 0, true))
					return
				}
			} else {
				failures = 0
				if !send(frames, browserSocketFrame{body: map[string]any{"type": "frame", "data": frame.JPEG, "tab": frame.Tab, "meta": map[string]int{"w": browser.ViewportWidth, "h": browser.ViewportHeight}}}) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	ticker := time.NewTicker(browserScreenHeartbeat)
	defer ticker.Stop()
	for {
		var frame browserSocketFrame
		select {
		case <-ctx.Done():
			return
		case frame = <-replies:
		case frame = <-frames:
		case <-ticker.C:
			if err = authenticate(); err != nil {
				return
			}
			operation, stop := context.WithTimeout(ctx, browserSocketTimeout)
			err = connection.Ping(operation)
			stop()
			if err != nil {
				return
			}
			continue
		}
		// 每张画面发送前再次认证，撤销设备后不再泄露页面内容。
		if err = authenticate(); err != nil {
			frame = browserScreenError(err, 0, true)
		}
		if err = write(frame.body); err != nil || frame.stop {
			return
		}
	}
}
