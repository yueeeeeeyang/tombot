package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/workspace"
)

func integerQuery(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return 0, domain.NewError(400, "INVALID_INPUT", "The query parameter must be a positive integer.")
	}
	return number, nil
}
func (s *Server) fileRoutes(r chi.Router) {
	r.Get("/fs/tree", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		depth, err := integerQuery(r, "depth", 2)
		if err != nil {
			return nil, err
		}
		query := r.URL.Query()
		return s.Files.Tree(ctx, query.Get("bot_id"), query.Get("scope"), query.Get("path"), depth)
	}))
	r.Get("/fs/file", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		query := r.URL.Query()
		if query.Has("start_line") || query.Has("limit") {
			return nil, domain.NewError(400, "INVALID_INPUT", "File previews return complete text and do not accept line pagination.")
		}
		return s.Files.Read(ctx, query.Get("bot_id"), query.Get("scope"), query.Get("path"))
	}))
	r.Put("/fs/file", s.writeFile)
	r.Post("/fs/upload", s.uploadFile)
	r.Get("/fs/download", s.downloadFile)
	r.Head("/fs/download", s.downloadFile)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	var input service.FileWriteInput
	// JSON 转义最多把一个文本字节扩成六字节，容量按解码后的文本另行校验。
	if err := decodeLimit(w, r, &input, 6*workspace.MaxTextBytes+16<<10); err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.Files.Write(r.Context(), input, actor(r))
	if err != nil {
		if result.Path != "" {
			s.Logger.Error("文件已写入，但收尾失败", "bot_id", input.BotID, "request_id", r.Context().Value(requestKey))
		}
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 200, result)
}
