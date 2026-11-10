package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
)

// streamWorkspaceMessage 把原来的 agent 运行过程推给桌面。流式开关只影响客户端是否逐段绘制。
func (s *Server) streamWorkspaceMessage(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil || s.Runtime == nil || s.Bus == nil {
		if err == nil {
			err = domain.NewError(500, "AGENT_UNAVAILABLE", "服务端 agent 还没有启动。")
		}
		s.fail(w, r, err)
		return
	}
	var request struct {
		AgentID   string `json:"agentId"`
		MessageID string `json:"messageId"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	dispatch, err := s.Workspace.PrepareDispatch(r.Context(), request.AgentID, request.MessageID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if dispatch.Done {
		s.respond(w, r, http.StatusOK, dispatch.Result)
		return
	}
	cursor, err := s.eventCursor(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	submission, err := s.Runtime.Submit(r.Context(), dispatch.ConversationID, agent.SendInput{Content: dispatch.Content, Attachments: dispatch.Attachments, ReplyToID: dispatch.ReplyToID}, "desktop")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(event, payload string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
			return err
		}
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+payload+"\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	}
	// 接受之后客户端断开也不取消运行。结果写回工作空间，再尽量把完成事件送回去。
	if err = write("accepted", fmt.Sprintf(`{"turnId":%q}`, submission.TurnID)); err != nil && s.Logger != nil {
		s.Logger.Info("桌面连接已断开，agent 继续运行", "agent_id", dispatch.AgentID, "message_id", dispatch.MessageID, "turn_id", submission.TurnID)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reply, runErr := s.followRun(runCtx, dispatch.ConversationID, submission.TurnID, cursor, write)
	delivery, applyErr := s.Workspace.ApplyRun(context.Background(), dispatch.AgentID, dispatch.MessageID, dispatch.Revision, reply, runErr)
	if applyErr != nil {
		if s.Logger != nil {
			s.Logger.Error("agent 结果没有写入工作空间", "agent_id", dispatch.AgentID, "message_id", dispatch.MessageID, "error", applyErr)
		}
		code := "WORKSPACE_CONFLICT"
		var failure *domain.Error
		if errors.As(applyErr, &failure) && failure.Code != "" {
			code = failure.Code
		}
		_ = write("error", fmt.Sprintf(`{"code":%q,"message":%q}`, code, applyErr.Error()))
		return
	}
	encoded, err := json.Marshal(delivery)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("agent 结果无法编码", "agent_id", dispatch.AgentID, "message_id", dispatch.MessageID, "error", err)
		}
		return
	}
	if err = write("done", string(encoded)); err != nil && s.Logger != nil {
		s.Logger.Info("桌面已断开，运行结果已保存", "agent_id", dispatch.AgentID, "message_id", dispatch.MessageID, "turn_id", submission.TurnID)
	}
}

// postConversationMessage 接受原来的发送体。桌面用事件流看过程，其他调用仍拿到 202。
func (s *Server) postConversationMessage(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamConversationMessage(w, r)
		return
	}
	handleInput(s, 202, func(ctx context.Context, r *http.Request, input agent.SendInput) (any, error) {
		return s.Runtime.Submit(ctx, chi.URLParam(r, "id"), input, actor(r))
	})(w, r)
}

func (s *Server) streamConversationMessage(w http.ResponseWriter, r *http.Request) {
	if s.Runtime == nil || s.Bus == nil {
		s.fail(w, r, domain.NewError(500, "AGENT_UNAVAILABLE", "服务端 agent 还没有启动。"))
		return
	}
	var input agent.SendInput
	if err := decode(w, r, &input); err != nil {
		s.fail(w, r, err)
		return
	}
	cursor, err := s.eventCursor(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	submission, err := s.Runtime.Submit(r.Context(), chi.URLParam(r, "id"), input, actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(event, payload string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
			return err
		}
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+payload+"\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err = write("accepted", fmt.Sprintf(`{"turnId":%q,"messageId":%q}`, submission.TurnID, submission.MessageID)); err != nil && s.Logger != nil {
		s.Logger.Info("桌面连接已断开，群聊继续运行", "conversation_id", chi.URLParam(r, "id"), "turn_id", submission.TurnID)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reply, runErr := s.followRun(runCtx, chi.URLParam(r, "id"), submission.TurnID, cursor, write)
	if runErr != nil {
		code := "MODEL_FAILED"
		var failure *domain.Error
		if errors.As(runErr, &failure) && failure.Code != "" {
			code = failure.Code
		}
		_ = write("error", fmt.Sprintf(`{"code":%q,"message":%q}`, code, runErr.Error()))
		return
	}
	encoded, err := json.Marshal(map[string]string{"reply": reply, "messageId": submission.MessageID})
	if err != nil {
		return
	}
	_ = write("done", string(encoded))
}

func (s *Server) setMessageReaction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emoji string `json:"emoji"`
	}
	if err := decode(w, r, &input); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Conversations.ToggleReaction(r.Context(), chi.URLParam(r, "id"), input.Emoji); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, map[string]bool{"toggled": true})
}

func (s *Server) ensureDesktopAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Workspace.EnsureAgent(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, map[string]bool{"ready": true})
}

// eventCursor 停在当前最后一条事件。新的运行只消费这之后的增量，避免把上一轮回复再拼进来。
func (s *Server) eventCursor(ctx context.Context) (int64, error) {
	var cursor int64
	for {
		events, reset, err := s.Bus.Read(ctx, cursor)
		if err != nil {
			return 0, err
		}
		if reset {
			return 0, domain.NewError(500, "EVENT_RESET", "运行事件已过期，请重新发送。")
		}
		if len(events) == 0 {
			return cursor, nil
		}
		cursor = events[len(events)-1].ID
		if len(events) < 200 {
			return cursor, nil
		}
	}
}

func (s *Server) followRun(ctx context.Context, conversationID, turnID string, cursor int64, write func(string, string) error) (string, error) {
	var reply string
	clientClosed := false
	// 写出失败只说明桌面不再接收，运行本身继续，直到回合结束再落库。
	emit := func(event, payload string) {
		if clientClosed {
			return
		}
		if err := write(event, payload); err != nil {
			clientClosed = true
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			if reply == "" {
				return reply, domain.NewError(504, "RUN_TIMEOUT", "agent 运行超时。")
			}
			return reply, err
		}
		wake := s.Bus.Wake()
		events, reset, err := s.Bus.Read(ctx, cursor)
		if err != nil {
			return reply, err
		}
		if reset {
			return reply, domain.NewError(500, "EVENT_RESET", "运行事件已过期，请重新发送。")
		}
		for _, event := range events {
			cursor = event.ID
			if event.ConversationID != conversationID {
				continue
			}
			switch event.Kind {
			case "text.delta":
				var payload struct {
					Text string `json:"text"`
				}
				if err = json.Unmarshal(event.Payload, &payload); err != nil {
					return reply, err
				}
				reply += payload.Text
				emit("delta", fmt.Sprintf(`{"text":%q}`, payload.Text))
			case "thinking.delta":
				var payload struct {
					Text string `json:"text"`
				}
				if err = json.Unmarshal(event.Payload, &payload); err != nil {
					return reply, err
				}
				if payload.Text != "" {
					emit("process", fmt.Sprintf(`{"text":%q}`, payload.Text))
				}
			case "tool.started":
				emit("process", `{"text":"正在使用工具"}`)
			case "turn.finished":
				var payload struct {
					TurnID string `json:"turn_id"`
					Status string `json:"status"`
					Code   string `json:"error_code"`
				}
				if err = json.Unmarshal(event.Payload, &payload); err != nil {
					return reply, err
				}
				if payload.TurnID != turnID {
					continue
				}
				if payload.Status == "succeeded" || payload.Status == "completed" {
					return reply, nil
				}
				if payload.Code == "" {
					payload.Code = payload.Status
				}
				return reply, domain.NewError(502, payload.Code, "agent 运行没有完成。")
			}
		}
		select {
		case <-ctx.Done():
			if reply == "" {
				return reply, domain.NewError(504, "RUN_TIMEOUT", "agent 运行超时。")
			}
			return reply, ctx.Err()
		case <-wake:
		case <-time.After(200 * time.Millisecond):
		}
	}
}
