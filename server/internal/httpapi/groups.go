package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

func groupRevision(r *http.Request) (int64, error) {
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 1 {
		return 0, domain.NewError(400, "INVALID_INPUT", "Provide the current group revision.")
	}
	return revision, nil
}
func (s *Server) groupRoutes(r chi.Router) {
	r.Get("/groups", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Groups.List(ctx, r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/groups", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.GroupInput) (any, error) {
		return s.Groups.Create(ctx, input, actor(r))
	}))
	r.Get("/groups/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Groups.Get(ctx, chi.URLParam(r, "id"))
	}))
	r.Patch("/groups/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.GroupPatch) (any, error) {
		return s.Groups.Update(ctx, chi.URLParam(r, "id"), input, actor(r))
	}))
	r.Post("/groups/{id}/members", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.GroupMembersInput) (any, error) {
		return s.Groups.ChangeMembers(ctx, chi.URLParam(r, "id"), input, "", actor(r))
	}))
	r.Delete("/groups/{id}/members/{botID}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		revision, err := groupRevision(r)
		if err != nil {
			return nil, err
		}
		return s.Groups.ChangeMembers(ctx, chi.URLParam(r, "id"), service.GroupMembersInput{Revision: revision}, chi.URLParam(r, "botID"), actor(r))
	}))
	r.Delete("/groups/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		revision, err := groupRevision(r)
		if err != nil {
			return err
		}
		return s.Groups.Delete(ctx, chi.URLParam(r, "id"), revision, actor(r))
	}))
}
