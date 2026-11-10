package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/workspace"
)

type browserControlInput struct {
	Controller string `json:"controller"`
	Revision   uint64 `json:"revision"`
}
type browserRestoreInput struct {
	Revision uint64 `json:"revision"`
}

func (s *Server) browserRoutes(r chi.Router) {
	r.Get("/browser/status", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) { return s.Browser.Status(ctx) }))
	r.Get("/browser/tabs", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Browser.List(ctx, r.URL.Query().Get("bot_id"))
	}))
	r.Post("/browser/tabs", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.BrowserOpenInput) (any, error) {
		return s.Browser.Open(ctx, input, nil, actor(r))
	}))
	r.Get("/browser/tabs/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Browser.Get(ctx, chi.URLParam(r, "id"))
	}))
	r.Get("/browser/tabs/{id}/snapshot", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Browser.Snapshot(ctx, chi.URLParam(r, "id"), nil)
	}))
	r.Get("/browser/tabs/{id}/screenshot", s.browserScreenshot)
	r.Get("/browser/tabs/{id}/screen", s.browserScreenSocket)
	r.Post("/browser/tabs/{id}/downloads", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.BrowserDownloadInput) (any, error) {
		return s.Browser.Download(ctx, chi.URLParam(r, "id"), input, nil, actor(r), func(ctx context.Context, botID, name string, source io.Reader) (workspace.WriteResult, error) {
			return s.Files.SaveBrowserDownload(ctx, botID, name, source, actor(r))
		})
	}))
	r.Post("/browser/tabs/{id}/actions", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.BrowserAction) (any, error) {
		return s.Browser.Apply(ctx, chi.URLParam(r, "id"), input, nil, actor(r))
	}))
	r.Post("/browser/tabs/{id}/control", handleInput(s, 200, func(ctx context.Context, r *http.Request, input browserControlInput) (any, error) {
		return s.Browser.SetController(ctx, chi.URLParam(r, "id"), input.Controller, input.Revision, actor(r))
	}))
	r.Post("/browser/tabs/{id}/restore", handleInput(s, 200, func(ctx context.Context, r *http.Request, input browserRestoreInput) (any, error) {
		return s.Browser.RestoreTab(ctx, chi.URLParam(r, "id"), input.Revision, actor(r))
	}))
	r.Delete("/browser/tabs/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		revision, err := strconv.ParseUint(r.URL.Query().Get("revision"), 10, 64)
		if err != nil || revision == 0 {
			return domain.NewError(400, "INVALID_BROWSER_INPUT", "Provide the current controller revision.")
		}
		return s.Browser.CloseTab(ctx, chi.URLParam(r, "id"), service.BrowserVersion{Revision: revision}, nil, actor(r))
	}))
	r.Post("/browser/recover", handleInput(s, 200, func(ctx context.Context, r *http.Request, _ struct{}) (any, error) {
		err := s.Browser.Recover(ctx, actor(r))
		return map[string]bool{"recovered": err == nil}, err
	}))
}

func (s *Server) browserScreenshot(w http.ResponseWriter, r *http.Request) {
	frame, err := s.Browser.CaptureScreen(r.Context(), chi.URLParam(r, "id"), 60)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(frame.JPEG)))
	origin, err := browser.Origin(frame.Tab.URL)
	if err != nil {
		origin = "about:blank"
	}
	w.Header().Set("X-Browser-Origin", origin)
	w.Header().Set("X-Browser-Error", frame.Tab.ErrorCode)
	if _, err = w.Write(frame.JPEG); err != nil {
		s.Logger.Warn("浏览器截图传输中断", "tab_id", chi.URLParam(r, "id"), "request_id", r.Context().Value(requestKey))
	}
}
