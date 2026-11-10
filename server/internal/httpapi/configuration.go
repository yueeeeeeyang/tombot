package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func actor(r *http.Request) string { return "device:" + r.Context().Value(deviceKey).(string) }
func pageLimit(r *http.Request, fallback int) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 200 {
		return 0, domain.NewError(400, "INVALID_LIMIT", "Limit must be between 1 and 200.")
	}
	return limit, nil
}
func handleInput[T any](s *Server, status int, execute func(context.Context, *http.Request, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input T
		if err := decode(w, r, &input); err != nil {
			s.fail(w, r, err)
			return
		}
		value, err := execute(r.Context(), r, input)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, r, status, value)
	}
}
func handleRead(s *Server, execute func(context.Context, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		value, err := execute(r.Context(), r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, r, 200, value)
	}
}
func handleDelete(s *Server, execute func(context.Context, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := execute(r.Context(), r); err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(204)
	}
}

// configurationRoutes 只处理契约转换；所有验证、事务与副作用位于领域服务。
func (s *Server) configurationRoutes(r chi.Router) {
	s.searchRoutes(r)
	r.Get("/settings", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) { return s.Settings.Get(ctx) }))
	r.Patch("/settings", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.SettingsPatch) (any, error) {
		settings, err := s.Settings.Update(ctx, input, actor(r))
		if err == nil {
			s.Bus.Notify()
		}
		return settings, err
	}))
	r.Get("/notifications/status", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Approvals.NotificationStatus(ctx)
	}))
	r.Get("/approvals", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Approvals.List(ctx, r.URL.Query().Get("status"), r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/approvals/{id}/decision", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.ApprovalDecision) (any, error) {
		err := s.Approvals.Decide(ctx, chi.URLParam(r, "id"), input, actor(r))
		return map[string]bool{"resolved": err == nil}, err
	}))
	r.Get("/policy-rules", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		return s.Approvals.Rules(ctx, r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/policy-rules", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.PolicyRule) (any, error) {
		return s.Approvals.SaveRule(ctx, input, actor(r))
	}))
	r.Delete("/policy-rules/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Approvals.DeleteRule(ctx, chi.URLParam(r, "id"), actor(r))
	}))
	r.Get("/tools", handleRead(s, func(context.Context, *http.Request) (any, error) { return service.ToolNames(), nil }))
	r.Get("/provider-presets", handleRead(s, func(context.Context, *http.Request) (any, error) { return service.ProviderPresets(), nil }))
	r.Post("/providers/connect", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.ProviderConnectInput) (any, error) {
		result, err := s.Catalog.ConnectProvider(ctx, input, actor(r), s.Secrets)
		if err == nil {
			s.Bus.Notify()
		}
		return result, err
	}))
	r.Get("/providers", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 100)
		if err != nil {
			return nil, err
		}
		return s.Catalog.Providers(ctx, r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/providers", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.ProviderInput) (any, error) {
		return s.Catalog.CreateProvider(ctx, input, actor(r))
	}))
	r.Get("/providers/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Catalog.Provider(ctx, chi.URLParam(r, "id"))
	}))
	r.Patch("/providers/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.ProviderPatch) (any, error) {
		return s.Catalog.UpdateProvider(ctx, chi.URLParam(r, "id"), input, actor(r))
	}))
	r.Delete("/providers/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Catalog.DeleteProvider(ctx, chi.URLParam(r, "id"), actor(r))
	}))
	r.Post("/providers/{id}/test", s.testProvider)
	r.Get("/models", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 100)
		if err != nil {
			return nil, err
		}
		return s.Catalog.Models(ctx, r.URL.Query().Get("cursor"), limit)
	}))
	r.Post("/models", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.ModelInput) (any, error) {
		model, err := s.Catalog.CreateModel(ctx, input, actor(r))
		if err == nil {
			s.Bus.Notify()
		}
		return model, err
	}))
	r.Get("/models/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Catalog.Model(ctx, chi.URLParam(r, "id"))
	}))
	r.Patch("/models/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.ModelPatch) (any, error) {
		model, err := s.Catalog.UpdateModel(ctx, chi.URLParam(r, "id"), input, actor(r))
		if err == nil {
			s.Bus.Notify()
		}
		return model, err
	}))
	r.Delete("/models/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Catalog.DeleteModel(ctx, chi.URLParam(r, "id"), actor(r))
	}))
	r.Get("/secrets", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 100)
		if err != nil {
			return nil, err
		}
		return s.Secrets.List(ctx, r.URL.Query().Get("cursor"), limit)
	}))
	type secretInput struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	r.Post("/secrets", handleInput(s, 201, func(ctx context.Context, r *http.Request, input secretInput) (any, error) {
		return s.Secrets.Create(ctx, input.Name, input.Value, actor(r))
	}))
	type rotateInput struct {
		Value string `json:"value"`
	}
	r.Post("/secrets/{id}/rotate", handleInput(s, 200, func(ctx context.Context, r *http.Request, input rotateInput) (any, error) {
		err := s.Secrets.Rotate(ctx, chi.URLParam(r, "id"), input.Value, actor(r))
		return map[string]bool{"rotated": err == nil}, err
	}))
	r.Delete("/secrets/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Secrets.Delete(ctx, chi.URLParam(r, "id"), actor(r))
	}))
	r.Get("/bots/templates", handleRead(s, func(context.Context, *http.Request) (any, error) { return service.BotTemplates(), nil }))
	r.Get("/bots", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 100)
		if err != nil {
			return nil, err
		}
		return s.Bots.List(ctx, r.URL.Query().Get("cursor"), limit, r.URL.Query().Get("archived") == "true")
	}))
	r.Post("/bots", handleInput(s, 201, func(ctx context.Context, r *http.Request, input service.BotInput) (any, error) {
		return s.Bots.Create(ctx, input, actor(r))
	}))
	r.Get("/bots/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) { return s.Bots.Get(ctx, chi.URLParam(r, "id")) }))
	r.Patch("/bots/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.BotPatch) (any, error) {
		return s.Bots.Update(ctx, chi.URLParam(r, "id"), input, actor(r))
	}))
	r.Delete("/bots/{id}", handleDelete(s, func(ctx context.Context, r *http.Request) error {
		return s.Bots.Delete(ctx, chi.URLParam(r, "id"), r.URL.Query().Get("confirm_name"), actor(r), r.URL.Query().Get("purge_workspace") == "true")
	}))
	r.Post("/bots/{id}/avatar", s.uploadAvatar)
	r.Get("/bots/{id}/avatar", s.readAvatar)
	r.Get("/conversations", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		limit, err := pageLimit(r, 20)
		if err != nil {
			return nil, err
		}
		return s.Conversations.List(ctx, r.URL.Query().Get("kind"), r.URL.Query().Get("cursor"), limit)
	}))
	type directInput struct {
		Kind  string `json:"kind"`
		BotID string `json:"bot_id"`
	}
	r.Post("/conversations", handleInput(s, 200, func(ctx context.Context, r *http.Request, input directInput) (any, error) {
		if input.Kind != "direct" {
			return nil, domain.NewError(400, "INVALID_INPUT", "Use the group API to create group conversations.")
		}
		return s.Conversations.Direct(ctx, input.BotID)
	}))
	r.Get("/conversations/{id}", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		return s.Conversations.Snapshot(ctx, chi.URLParam(r, "id"))
	}))
	r.Patch("/conversations/{id}", handleInput(s, 200, func(ctx context.Context, r *http.Request, input service.ConversationPatch) (any, error) {
		return s.Conversations.Update(ctx, chi.URLParam(r, "id"), input, actor(r))
	}))
	r.Get("/conversations/{id}/messages", handleRead(s, s.readConversationMessages))

	type readInput struct {
		Seq int64 `json:"seq"`
	}
	r.Post("/conversations/{id}/read", handleInput(s, 200, func(ctx context.Context, r *http.Request, input readInput) (any, error) {
		err := s.Conversations.MarkRead(ctx, chi.URLParam(r, "id"), input.Seq)
		return map[string]bool{"read": err == nil}, err
	}))
}
func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ModelKey string `json:"model_key"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &input); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	result, err := s.Catalog.Probe(r.Context(), chi.URLParam(r, "id"), input.ModelKey, actor(r), s.Secrets, func(p domain.Provider, key []byte) provider.Provider { return provider.NewHTTP(p, key) })
	s.Bus.Notify()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 200, result)
}
func (s *Server) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	if err := r.ParseMultipartForm(5 << 20); err != nil {
		s.fail(w, r, domain.NewError(400, "INVALID_AVATAR", "Provide a PNG or JPEG multipart file."))
		return
	}
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			s.Logger.Warn("头像上传临时文件清理失败", "request_id", r.Context().Value(requestKey))
		}
	}()
	file, _, err := r.FormFile("file")
	if err != nil {
		s.fail(w, r, domain.NewError(400, "INVALID_AVATAR", "Avatar field 'file' is required."))
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			s.Logger.Warn("头像上传文件关闭失败", "request_id", r.Context().Value(requestKey))
		}
	}()
	path, err := s.Bots.SetAvatar(r.Context(), chi.URLParam(r, "id"), actor(r), file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 200, map[string]string{"avatar_path": path})
}
func (s *Server) readAvatar(w http.ResponseWriter, r *http.Request) {
	file, err := s.Bots.Avatar(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			s.Logger.Warn("头像读取文件关闭失败", "request_id", r.Context().Value(requestKey))
		}
	}()
	stat, err := file.Stat()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "avatar.png", stat.ModTime(), file)
}
