package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/memory"
)

type memoryEdit struct {
	domain.MemoryInput
	ExpectedSHA256 string `json:"expected_sha256"`
}

func (s *Server) memoryRoutes(r chi.Router) {
	r.Get("/bots/{id}/memory", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		if query := r.URL.Query().Get("query"); query != "" {
			limit, err := integerQuery(r, "limit", 5)
			if err != nil {
				return nil, err
			}
			return s.Memories.Search(ctx, chi.URLParam(r, "id"), query, limit)
		}
		return s.Memories.List(ctx, chi.URLParam(r, "id"))
	}))
	r.Post("/bots/{id}/memory", s.createMemory)
	r.Post("/bots/{id}/memory/reindex", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		entries, err := s.Memories.Sync(ctx, chi.URLParam(r, "id"), true)
		return map[string]int{"count": len(entries)}, err
	}))
	r.Get("/memory/{botId}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Memories.Read(ctx, chi.URLParam(r, "botId"), r.URL.Query().Get("path"))
	}))
	r.Patch("/memory/{botId}", s.updateMemory)
	r.Delete("/memory/{botId}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Memories.Delete(ctx, chi.URLParam(r, "botId"), r.URL.Query().Get("path"), r.URL.Query().Get("expected_sha256"), actor(r))
	}))
}

func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) {
	var input domain.MemoryInput
	if err := decodeLimit(w, r, &input, 6*domain.MaxMemoryFileBytes+16<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	entry, err := s.Memories.Write(r.Context(), chi.URLParam(r, "id"), "", input, "", actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 201, entry)
}
func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !memory.ValidPath(path) {
		s.fail(w, r, domain.NewError(400, "MEMORY_PATH_INVALID", "Editing requires the current private memory path."))
		return
	}
	var input memoryEdit
	if err := decodeLimit(w, r, &input, 6*domain.MaxMemoryFileBytes+16<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	if input.ExpectedSHA256 == "" {
		s.fail(w, r, domain.NewError(400, "INVALID_INPUT", "Supply the current memory SHA-256 before editing."))
		return
	}
	entry, err := s.Memories.Write(r.Context(), chi.URLParam(r, "botId"), path, input.MemoryInput, input.ExpectedSHA256, actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 200, entry)
}
