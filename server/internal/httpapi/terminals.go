package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/term"
)

const maximumTerminalConnections = 128
const terminalSocketTimeout = 5 * time.Second
const terminalHeartbeatInterval = 15 * time.Second

type terminalControlInput struct {
	Controller string `json:"controller"`
}

func (s *Server) terminalRoutes(r chi.Router) {
	r.Get("/terminals", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Terminals.List(ctx, r.URL.Query().Get("bot_id"), r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/terminals", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.TerminalOpenInput) (any, error) {
		return s.Terminals.Open(ctx, input, nil, actor(r))
	}))
	r.Post("/terminals/{id}/control", handleInput(s, 200, func(ctx context.Context, r *http.Request, input terminalControlInput) (any, error) {
		return s.Terminals.SetController(ctx, chi.URLParam(r, "id"), input.Controller, actor(r))
	}))
	r.Delete("/terminals/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		err := s.Terminals.Close(ctx, chi.URLParam(r, "id"), nil, 0, actor(r))
		return map[string]bool{"closed": err == nil}, err
	}))
	r.Get("/terminals/{id}", s.terminalSocket)
}

type terminalInputFrame struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Columns  int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Sequence int64  `json:"seq,omitempty"`
}
type terminalFrame struct {
	body map[string]any
	stop bool
}

func parseTerminalInput(payload []byte) (terminalInputFrame, error) {
	var input terminalInputFrame
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Provide a supported terminal JSON frame.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Provide exactly one terminal JSON frame.")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Provide a terminal JSON object.")
	}
	for name, value := range fields {
		if string(value) == "null" || input.Type == "input" && (name == "cols" || name == "rows") || input.Type == "resize" && name == "data" {
			return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "The field is not valid for this terminal frame.")
		}
	}
	if input.Sequence < 0 || input.Sequence > 1<<53 {
		return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Invalid input sequence.")
	}
	switch input.Type {
	case "input":
		if input.Columns != 0 || input.Rows != 0 {
			return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Input frames cannot resize the terminal.")
		}
		return input, term.ValidateSessionInput(input.Data)
	case "resize":
		if input.Data != "" {
			return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Resize frames cannot include input data.")
		}
		return input, term.ValidateSize(input.Columns, input.Rows)
	default:
		return input, domain.NewError(400, "INVALID_TERMINAL_FRAME", "Frame type must be input or resize.")
	}
}

func terminalErrorFrame(err error, sequence int64) terminalFrame {
	var failure *domain.Error
	if !errors.As(err, &failure) {
		failure = domain.NewError(500, "TERMINAL_IO_FAILED", "The terminal operation failed. Do not retry input automatically.")
	}
	return terminalFrame{body: map[string]any{"type": "error", "code": failure.Code, "message": failure.Message, "seq": sequence}}
}

// terminalSocket 每连接保留独立输出游标；认证失效、慢消费者或根上下文取消只断开连接。
func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	terminal, err := s.Terminals.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.respond(w, r, 200, terminal)
		return
	}
	cursor := int64(0)
	if value := r.URL.Query().Get("after"); value != "" {
		cursor, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cursor < 0 {
			s.fail(w, r, domain.NewError(400, "INVALID_TERMINAL_CURSOR", "Use a nonnegative output cursor."))
			return
		}
	}
	readonly := r.URL.Query().Get("readonly")
	if readonly != "" && readonly != "0" && readonly != "1" {
		s.fail(w, r, domain.NewError(400, "INVALID_INPUT", "Readonly must be 0 or 1."))
		return
	}
	if _, err = s.Terminals.Output(r.Context(), id, cursor, 4, false); err != nil {
		s.fail(w, r, err)
		return
	}
	connections := s.terminalConnections.Add(1)
	defer s.terminalConnections.Add(-1)
	if connections > maximumTerminalConnections {
		s.fail(w, r, domain.NewError(429, "TERMINAL_CONNECTION_LIMIT", "Too many terminal connections are open."))
		return
	}
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.Logger.Debug("终端连接升级失败", "terminal_id", id, "request_id", r.Context().Value(requestKey))
		return
	}
	connection.SetReadLimit(6*term.MaxSessionInputBytes + 512)
	ctx, cancel := context.WithCancel(r.Context())
	stopRoot := context.AfterFunc(s.RootContext, cancel)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		stopRoot()
		if err := connection.CloseNow(); err != nil {
			s.Logger.Debug("终端连接已断开", "terminal_id", id)
		}
		workers.Wait()
	}()
	write := func(frame map[string]any) error {
		operation, stop := context.WithTimeout(ctx, terminalSocketTimeout)
		defer stop()
		return wsjson.Write(operation, connection, frame)
	}
	if err = write(map[string]any{"type": "ready", "terminal": terminal, "readonly": readonly == "1"}); err != nil {
		return
	}
	frames := make(chan terminalFrame, 2)
	send := func(frame terminalFrame) bool {
		select {
		case <-ctx.Done():
			return false
		case frames <- frame:
			return true
		}
	}
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer cancel()
		for {
			kind, payload, err := connection.Read(ctx)
			if err != nil {
				return
			}
			input, parseErr := parseTerminalInput(payload)
			if kind != websocket.MessageText {
				parseErr = domain.NewError(400, "INVALID_TERMINAL_FRAME", "Terminal input must be a text JSON frame.")
			}
			if parseErr != nil {
				if !send(terminalErrorFrame(parseErr, input.Sequence)) {
					return
				}
				continue
			}
			operation, stop := context.WithTimeout(ctx, terminalSocketTimeout)
			_, authErr := s.Auth.Authenticate(operation, bearer(r))
			if authErr != nil {
				stop()
				frame := terminalErrorFrame(authErr, input.Sequence)
				frame.stop = true
				if !send(frame) {
					return
				}
				// 等待写端送达错误后关闭，避免取消抢在错误帧之前。
				<-ctx.Done()
				return
			}
			if readonly == "1" {
				err = domain.NewError(403, "TERMINAL_READONLY", "This terminal connection is read-only.")
			} else if input.Type == "input" {
				err = s.Terminals.Write(operation, id, input.Data, nil, 0, actor(r))
			} else {
				err = s.Terminals.Resize(operation, id, input.Columns, input.Rows, nil, 0, actor(r))
			}
			stop()
			if err != nil {
				var expected *domain.Error
				if !errors.As(err, &expected) {
					s.Logger.Error("终端输入处理失败", "terminal_id", id, "device_id", r.Context().Value(deviceKey))
				}
				if !send(terminalErrorFrame(err, input.Sequence)) {
					return
				}
			} else if !send(terminalFrame{body: map[string]any{"type": "ack", "action": input.Type, "seq": input.Sequence}}) {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			output, err := s.Terminals.Output(ctx, id, cursor, 16<<10, true)
			if err != nil {
				frame := terminalErrorFrame(err, 0)
				frame.stop = true
				send(frame)
				return
			}
			cursor = output.Cursor
			if output.Data != "" && !send(terminalFrame{body: map[string]any{"type": "output", "data": output.Data, "cursor": cursor, "reset": output.Truncated}}) {
				return
			}
			if output.Exited && !output.HasMore {
				send(terminalFrame{body: map[string]any{"type": "exit", "code": output.ExitCode, "error_code": output.ErrorCode, "cursor": cursor}, stop: true})
				return
			}
		}
	}()
	ticker := time.NewTicker(terminalHeartbeatInterval)
	defer ticker.Stop()
	wake := s.Bus.Wake()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-frames:
			if err = write(frame.body); err != nil {
				return
			}
			if frame.stop {
				return
			}
		case <-wake:
			wake = s.Bus.Wake()
			latest, loadErr := s.Terminals.Get(ctx, id)
			if loadErr != nil {
				if err = write(terminalErrorFrame(loadErr, 0).body); err != nil {
					s.Logger.Debug("终端状态发送失败", "terminal_id", id)
				}
				return
			}
			if latest.Revision != terminal.Revision || latest.Status != terminal.Status {
				terminal = latest
				if err = write(map[string]any{"type": "state", "terminal": terminal}); err != nil {
					return
				}
			}
		case <-ticker.C:
			operation, stop := context.WithTimeout(ctx, terminalSocketTimeout)
			_, err = s.Auth.Authenticate(operation, bearer(r))
			if err == nil {
				err = connection.Ping(operation)
			}
			stop()
			if err != nil {
				return
			}
		}
	}
}
