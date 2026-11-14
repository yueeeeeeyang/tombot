package nodeenrollment

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
)

type Registrar interface {
	Register(context.Context, Request, string, string) (Receipt, error)
}

type Handler struct {
	service Registrar
	logger  *slog.Logger
	slots   chan struct{}
}

func NewHandler(service Registrar, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("节点接入接口依赖无效")
	}
	return &Handler{service, logger, make(chan struct{}, 16)}, nil
}

// ServeHTTP 只处理签名接入请求；并发、正文和时限有界，响应及日志不回显凭据或原始输入。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost || r.URL.Path != Path || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case handler.slots <- struct{}{}:
		defer func() { <-handler.slots }()
	default:
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var request Request
	if failure := httpjson.Decode(r, &request, maximumBodyBytes); failure != nil {
		w.WriteHeader(failure.Status)
		return
	}
	token := ""
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) != 50 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		token = strings.TrimPrefix(authorization, "Bearer ")
	}
	requestID := ids.New()
	receipt, err := handler.service.Register(ctx, request, token, requestID)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrUnauthorized):
			status = http.StatusUnauthorized
		case errors.Is(err, ErrConflict), errors.Is(err, nodecatalog.ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, ErrCapacity):
			status = http.StatusConflict
		case errors.Is(err, nodecatalog.ErrInvalidInput):
			status = http.StatusBadRequest
		}
		handler.logger.WarnContext(ctx, "节点接入未确认", "request_id", requestID, "status", status)
		w.WriteHeader(status)
		return
	}
	if err := json.NewEncoder(w).Encode(receipt); err != nil {
		handler.logger.WarnContext(ctx, "节点接入回执发送未完成", "node_id", receipt.NodeID, "request_id", requestID)
	}
}
