package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) executionRoutes(r chi.Router) {
	r.Post("/turns/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Runtime.Cancel(r.Context(), chi.URLParam(r, "id"), actor(r)); err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, r, 202, map[string]bool{"accepted": true})
	})
	r.Get("/conversations/{id}/turns", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Runtime.Turns(ctx, chi.URLParam(r, "id"), r.URL.Query().Get("cursor"), limit)
	}))
	r.Get("/runs/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Runtime.RunDetail(ctx, chi.URLParam(r, "id"))
	}))
}
