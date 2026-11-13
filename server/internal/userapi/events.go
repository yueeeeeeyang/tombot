package userapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"io.yueyang/tom/internal/userevents"
)

// 事件流使用有界长轮询帧而不是无限连接：宿主写时限与请求时限都是分钟级，
// 在到期前主动收尾并给出重连提示，客户端按最后序号续订，不会读到半帧。
const (
	eventPollInterval    = time.Second
	eventHeartbeat       = 15 * time.Second
	eventDrainMargin     = 2 * time.Second
	eventMinimumWindow   = 5 * time.Second
	eventReconnectMillis = 1000
)

// streamEvents 只读取当前用户已提交事件；断线续订由 Last-Event-ID 或 after 决定。
func (handler *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := handler.eventCursor(w, r)
	if !ok {
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 重连提示随首个帧发出；客户端按浏览器默认退避重新订阅。
	if _, err := w.Write([]byte("retry: " + strconv.Itoa(eventReconnectMillis) + "\n\n")); err != nil {
		return
	}
	if err := controller.Flush(); err != nil {
		handler.logger.WarnContext(r.Context(), "事件流无法开始推送", "request_id", current(r).attempt.RequestID, "user_id", current(r).principal.UserID)
		return
	}
	if err := handler.pumpEvents(r, w, controller, after, limit); err != nil {
		handler.logger.WarnContext(r.Context(), "事件流推送中断", "request_id", current(r).attempt.RequestID, "user_id", current(r).principal.UserID, "reason", err.Error())
	}
}

// pumpEvents 在本次连接允许的时间内持续读取；没有新事件时只发送心跳注释。
func (handler *Handler) pumpEvents(r *http.Request, w http.ResponseWriter, controller *http.ResponseController, after int64, limit int) error {
	ctx := r.Context()
	stopAt := handler.eventStop(ctx)
	lastWrite := time.Now()
	idle := time.NewTimer(handler.eventTiming.pollInterval)
	defer idle.Stop()
	for {
		events, err := handler.events.Read(ctx, current(r).principal, after, limit)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 响应头已经发出，不能再改状态码；错误帧让客户端停止续订并重新读取目录。
			code, message := "EVENT_STREAM_FAILED", "事件读取失败"
			if errors.Is(err, userevents.ErrCursorExpired) {
				// 游标早于清理边界：缺失的事件已经删除，只能重新读取快照。
				code, message = "EVENT_CURSOR_EXPIRED", "事件游标已过期，请重新同步"
			}
			data, marshalErr := json.Marshal(map[string]string{"code": code, "message": message})
			if marshalErr != nil {
				return marshalErr
			}
			if writeErr := handler.writeEventFrame(w, "error", after, data); writeErr != nil {
				return writeErr
			}
			return controller.Flush()
		}
		if len(events) > 0 {
			for _, event := range events {
				data, marshalErr := json.Marshal(event)
				if marshalErr != nil {
					return marshalErr
				}
				if writeErr := handler.writeEventFrame(w, event.Kind, event.Sequence, data); writeErr != nil {
					return writeErr
				}
				after = event.Sequence
			}
			if err = controller.Flush(); err != nil {
				return err
			}
			lastWrite = time.Now()
			continue
		}
		// 已经追平且到达本次窗口末尾才收尾，保证连接内至少读到一次。
		if !stopAt.IsZero() && !time.Now().Before(stopAt) {
			return controller.Flush()
		}
		if time.Since(lastWrite) >= handler.eventTiming.heartbeat {
			if _, err = w.Write([]byte(": keep-alive\n\n")); err != nil {
				return err
			}
			if err = controller.Flush(); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle.C:
			idle.Reset(handler.eventTiming.pollInterval)
		}
	}
}

func (handler *Handler) writeEventFrame(w http.ResponseWriter, kind string, sequence int64, data json.RawMessage) error {
	if _, err := w.Write([]byte("event: " + kind + "\nid: " + strconv.FormatInt(sequence, 10) + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n\n"))
	return err
}

// eventStop 在请求时限之前留出收尾余量；窗口过短时只做一次读取。
func (handler *Handler) eventStop(ctx context.Context) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	stopAt := deadline.Add(-handler.eventTiming.drainMargin)
	if time.Until(stopAt) < handler.eventTiming.minimumWindow {
		return time.Now()
	}
	return stopAt
}

// eventCursor 接受 after 与 limit；Last-Event-ID 与 after 只允许出现一个。
func (handler *Handler) eventCursor(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, userevents.ErrInvalidInput)
		return 0, 0, false
	}
	for key, values := range query {
		if len(values) != 1 || key != "after" && key != "limit" {
			handler.serviceError(w, r, userevents.ErrInvalidInput)
			return 0, 0, false
		}
	}
	headerValues := r.Header.Values("Last-Event-ID")
	if len(headerValues) > 1 {
		handler.serviceError(w, r, userevents.ErrInvalidInput)
		return 0, 0, false
	}
	after, limit := int64(0), userevents.DefaultBatch
	if value, exists := query["after"]; exists {
		if len(headerValues) == 1 && headerValues[0] != "" {
			handler.serviceError(w, r, userevents.ErrInvalidInput)
			return 0, 0, false
		}
		after, err = strconv.ParseInt(value[0], 10, 64)
		if err != nil || after < 0 || strconv.FormatInt(after, 10) != value[0] {
			handler.serviceError(w, r, userevents.ErrInvalidInput)
			return 0, 0, false
		}
	} else if len(headerValues) == 1 {
		after, err = strconv.ParseInt(headerValues[0], 10, 64)
		if err != nil || after < 0 || strconv.FormatInt(after, 10) != headerValues[0] {
			handler.serviceError(w, r, userevents.ErrInvalidInput)
			return 0, 0, false
		}
	}
	if value, exists := query["limit"]; exists {
		limit, err = strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > userevents.MaxBatch {
			handler.serviceError(w, r, userevents.ErrInvalidInput)
			return 0, 0, false
		}
	}
	return after, limit, true
}
