package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

// workspaceRoutes 挂在原来的 /api/v1 上，不要求主机引导完成。认证仍走原来的登录接口。
func (s *Server) workspaceRoutes(router chi.Router) {
	router.Get("/computer/view", s.computerView)
	router.Group(func(protected chi.Router) {
		protected.Use(s.authenticate)
		protected.Get("/workspace", s.desktopLoadWorkspace)
		protected.Put("/workspace", s.desktopSaveWorkspace)
		protected.Post("/messages/stream", s.streamWorkspaceMessage)
		protected.Get("/conversations", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
			limit, err := pageLimit(r, 50)
			if err != nil {
				return nil, err
			}
			return s.Conversations.List(ctx, r.URL.Query().Get("kind"), r.URL.Query().Get("cursor"), limit)
		}))
		protected.Get("/conversations/{id}/messages", handleRead(s, s.readConversationMessages))
		protected.Post("/conversations/{id}/messages", s.postConversationMessage)
		protected.Put("/messages/{id}/reaction", s.setMessageReaction)
		protected.Post("/workspace/agents/{id}/ready", s.ensureDesktopAgent)
		protected.Post("/computer", s.openComputer)
		protected.Get("/model-settings", s.desktopLoadSettings)
		protected.Put("/model-settings", s.desktopSaveSettings)
		protected.Get("/model-providers", s.desktopModelProviders)
		protected.Post("/model-providers/{id}/models", s.desktopListProviderModels)
	})
}

func (s *Server) requireDesktop() error {
	if s.Workspace == nil {
		return domain.NewError(500, "DESKTOP_UNAVAILABLE", "桌面服务还没有启动。")
	}
	return nil
}

func (s *Server) desktopLoadWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	snapshot, err := s.Workspace.Load(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, snapshot)
}

func (s *Server) desktopSaveWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	workspace, err := decodeWorkspace(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	snapshot, err := s.Workspace.Save(r.Context(), workspace)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, snapshot)
}

func (s *Server) desktopLoadSettings(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.Workspace.LoadModelForm(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, view)
}

func (s *Server) desktopSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	var request struct {
		service.ModelForm
		APIKey   string `json:"apiKey,omitempty"`
		ClearKey bool   `json:"clearApiKey,omitempty"`
	}
	if err := decodeLimit(w, r, &request, 64<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	if base, ok := service.BuiltinBaseURL(request.Provider, s.Config); ok {
		request.BaseURL = base
		if request.ProviderName == "" {
			for _, item := range service.BuiltinProviders() {
				if item.ID == request.Provider {
					request.ProviderName = item.Name
				}
			}
		}
	}
	view, err := s.Workspace.SaveModelForm(r.Context(), request.ModelForm, request.APIKey, request.ClearKey)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, view)
}

func (s *Server) desktopModelProviders(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, service.BuiltinProviders())
}

func (s *Server) desktopListProviderModels(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	var request struct {
		APIKey string `json:"apiKey"`
	}
	if err := decodeLimit(w, r, &request, 64<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	base, ok := service.BuiltinBaseURL(chi.URLParam(r, "id"), s.Config)
	if !ok {
		s.fail(w, r, domain.NewError(404, "PROVIDER_NOT_FOUND", "没有这个内置提供商。"))
		return
	}
	models, err := s.Workspace.ListBuiltinModels(r.Context(), request.APIKey, base)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	listed := make([]map[string]string, 0, len(models))
	for _, model := range models {
		listed = append(listed, map[string]string{"id": model.ID, "name": model.Name})
	}
	s.respond(w, r, http.StatusOK, map[string]any{"models": listed})
}

func decodeWorkspace(w http.ResponseWriter, r *http.Request) (service.Workspace, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20+1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return service.Workspace{}, domain.NewError(413, "REQUEST_TOO_LARGE", "The request exceeds its size limit.")
		}
		return service.Workspace{}, domain.NewError(400, "REQUEST_INVALID", "The request body is invalid.")
	}
	return service.ParseWorkspace(raw)
}
