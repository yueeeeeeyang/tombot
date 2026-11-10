package httpapi

import (
	"context"
	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/service"
	"net/http"
)

type routineRevision struct {
	Revision int64 `json:"revision"`
}

func (s *Server) routineRoutes(r chi.Router) {
	r.Get("/routines", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Routines.List(ctx, r.URL.Query().Get("bot_id"), r.URL.Query().Get("group_id"), r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/routines", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.RoutineInput) (any, error) {
		return s.Routines.Commit(ctx, input, actor(r))
	}))
	r.Get("/routines/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Routines.Get(ctx, chi.URLParam(r, "id"))
	}))
	r.Patch("/routines/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.RoutinePatch) (any, error) {
		return s.Routines.UpdateNow(ctx, chi.URLParam(r, "id"), input, actor(r))
	}))
	r.Delete("/routines/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		revision, err := integerQuery(r, "revision", 0)
		if err != nil {
			return err
		}
		return s.Routines.Delete(ctx, chi.URLParam(r, "id"), int64(revision), actor(r))
	}))
	r.Post("/routines/{id}/run-now", handleInput(s, 200, func(ctx context.Context, r *http.Request, input routineRevision) (any, error) {
		return s.Routines.RunNow(ctx, chi.URLParam(r, "id"), input.Revision, actor(r))
	}))
	r.Get("/routines/{id}/runs", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Routines.Runs(ctx, chi.URLParam(r, "id"), r.URL.Query().Get("cursor"), limit)
	}))
}
