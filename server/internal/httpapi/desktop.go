package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/desktop"
	"io.yueyang/tom/internal/domain"
)

// desktopRoutes 提供给现有桌面界面的接口。它不要求主机引导完成，因为界面还不使用浏览器和终端。
func (s *Server) desktopRoutes(router chi.Router) {
	router.Get("/session", s.desktopSession)
	router.Post("/setup", s.desktopSetup)
	router.Post("/login", s.desktopLogin)
	router.Group(func(protected chi.Router) {
		protected.Use(s.authenticate)
		protected.Get("/workspace", s.desktopLoadWorkspace)
		protected.Put("/workspace", s.desktopSaveWorkspace)
		protected.Post("/workspace/import", s.desktopImportWorkspace)
		protected.Post("/messages/deliver", s.desktopDeliver)
		protected.Get("/settings", s.desktopLoadSettings)
		protected.Put("/settings", s.desktopSaveSettings)
	})
}

func (s *Server) requireDesktop() error {
	if s.Desktop == nil {
		return domain.NewError(500, "DESKTOP_UNAVAILABLE", "桌面服务还没有启动。")
	}
	return nil
}

func (s *Server) desktopSession(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	authenticated := false
	if token := bearer(r); token != "" {
		if _, err := s.Auth.Authenticate(r.Context(), token); err == nil {
			authenticated = true
		}
	}
	s.respond(w, r, http.StatusOK, map[string]bool{"initialized": s.Auth.Initialized(), "authenticated": authenticated})
}

func (s *Server) desktopSetup(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	var request struct {
		SetupCode string `json:"setupCode"`
		Password  string `json:"password"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Auth.Setup(request.Password, strings.TrimSpace(request.SetupCode)); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Bots != nil {
		if err := s.Bots.EnsureDefault(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.Logger.Info("桌面初始化完成", "request_id", r.Context().Value(requestKey))
	s.respond(w, r, http.StatusCreated, map[string]bool{"initialized": true})
}

func (s *Server) desktopLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	var request struct {
		Password   string `json:"password"`
		DeviceName string `json:"deviceName"`
		Platform   string `json:"platform"`
	}
	if err := decode(w, r, &request); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Bots != nil && s.Auth.Initialized() {
		if err := s.Bots.EnsureDefault(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	session, err := s.Auth.Login(r.Context(), request.Password, request.DeviceName, request.Platform, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Logger.Info("桌面设备已登录", "device_id", session.DeviceID, "request_id", r.Context().Value(requestKey))
	s.respond(w, r, http.StatusOK, session)
}

func (s *Server) desktopLoadWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	snapshot, err := s.Desktop.Load(r.Context())
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
	snapshot, err := s.Desktop.Save(r.Context(), workspace)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, snapshot)
}

func (s *Server) desktopImportWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	workspace, err := decodeWorkspace(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	snapshot, err := s.Desktop.Import(r.Context(), workspace)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, snapshot)
}

func (s *Server) desktopDeliver(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
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
	delivery, err := s.Desktop.Deliver(r.Context(), request.AgentID, request.MessageID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, delivery)
}

func (s *Server) desktopLoadSettings(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDesktop(); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.Desktop.LoadSettings(r.Context())
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
		desktop.Settings
		APIKey   string `json:"apiKey,omitempty"`
		ClearKey bool   `json:"clearApiKey,omitempty"`
	}
	if err := decodeLimit(w, r, &request, 64<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.Desktop.SaveSettings(r.Context(), request.Settings, request.APIKey, request.ClearKey)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, view)
}

func decodeWorkspace(w http.ResponseWriter, r *http.Request) (desktop.Workspace, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20+1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return desktop.Workspace{}, domain.NewError(413, "REQUEST_TOO_LARGE", "The request exceeds its size limit.")
		}
		return desktop.Workspace{}, domain.NewError(400, "REQUEST_INVALID", "The request body is invalid.")
	}
	return desktop.ParseWorkspace(raw)
}
