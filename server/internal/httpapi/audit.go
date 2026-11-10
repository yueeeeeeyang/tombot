package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, err := pageLimit(r, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	page, err := s.Audit.List(ctx, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, http.StatusOK, page)
}

// exportAudit 分批读取再写网络，慢客户端下载不占用数据库单写连接。中途失败即中断，完整 JSON 只在成功时闭合。
func (s *Server) exportAudit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.fail(w, r, err)
		return
	}
	if err := s.Audit.BeginExport(ctx, actor(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.Audit.List(ctx, "", 100)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tom-audit.json"`)
	abort := func() {
		s.Logger.Warn("审计导出中断", "request_id", r.Context().Value(requestKey), "device_id", r.Context().Value(deviceKey))
		panic(http.ErrAbortHandler)
	}
	if _, err = io.WriteString(w, "["); err != nil {
		abort()
	}
	first := true
	encoder := json.NewEncoder(w)
	for {
		for _, record := range page.Records {
			if ctx.Err() != nil {
				abort()
			}
			if !first {
				if _, err = io.WriteString(w, ","); err != nil {
					abort()
				}
			}
			if err = encoder.Encode(record); err != nil {
				abort()
			}
			first = false
		}
		if page.NextCursor == "" {
			break
		}
		if _, err = s.Auth.Authenticate(ctx, bearer(r)); err != nil {
			abort()
		}
		page, err = s.Audit.List(ctx, page.NextCursor, 100)
		if err != nil {
			abort()
		}
	}
	if _, err = io.WriteString(w, "]\n"); err != nil {
		abort()
	}
}
