package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

func (s *Server) searchRoutes(r chi.Router) {
	r.Get("/search", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 20)
		if err != nil {
			return nil, err
		}
		return s.Conversations.Search(ctx, r.URL.Query().Get("q"), r.URL.Query().Get("cursor"), limit)
	}))
}

func (s *Server) readConversationMessages(ctx context.Context, r *http.Request) (any, error) {
	limit, err := pageLimit(r, 50)
	if err != nil {
		return nil, err
	}
	query := r.URL.Query()
	if query.Has("around_seq") && query.Has("before_seq") {
		return nil, domain.NewError(400, "INVALID_CURSOR", "Use either before_seq or around_seq.")
	}
	if query.Has("around_seq") {
		seq, err := strconv.ParseInt(query.Get("around_seq"), 10, 64)
		if err != nil || seq < 1 {
			return nil, domain.NewError(400, "INVALID_CURSOR", "Message cursor must be positive.")
		}
		return s.Conversations.MessagesAround(ctx, chi.URLParam(r, "id"), seq, limit)
	}
	var before int64
	if value := query.Get("before_seq"); value != "" {
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			return nil, domain.NewError(400, "INVALID_CURSOR", "Message cursor must be positive.")
		}
	}
	messages, err := s.Conversations.Messages(ctx, chi.URLParam(r, "id"), before, limit)
	if err != nil {
		return nil, err
	}
	return s.withConversationChrome(ctx, messages)
}

type viewedMessage struct {
	domain.Message
	ReplyToID string             `json:"reply_to_id,omitempty"`
	Reactions []service.Reaction `json:"reactions,omitempty"`
}

func (s *Server) withConversationChrome(ctx context.Context, messages []domain.Message) ([]viewedMessage, error) {
	viewed := make([]viewedMessage, len(messages))
	for index, message := range messages {
		item := viewedMessage{Message: message}
		if s.Workspace != nil {
			reply, reactions, err := s.Workspace.MessageChrome(ctx, message.ID)
			if err != nil {
				return nil, err
			}
			item.ReplyToID = reply
			if len(reactions) > 0 {
				item.Reactions = reactions
			}
		}
		viewed[index] = item
	}
	return viewed, nil
}
