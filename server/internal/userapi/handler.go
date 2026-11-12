// Package userapi 提供登录用户的资源接口；用户身份只能来自认证门。
package userapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/execution"
	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/identity"
)

// Catalog 仅含所属用户的目录动作，不提供模型管理、主机路径或管理员越权读取入口。
type Catalog interface {
	Get(context.Context, identity.Principal, string) (bots.Bot, error)
	List(context.Context, identity.Principal, string, int, bool) ([]bots.Bot, error)
	Create(context.Context, identity.Principal, bots.CreateRequest) (bots.Bot, bool, error)
	Update(context.Context, identity.Principal, string, bots.UpdateRequest) (bots.Bot, error)
	SetArchived(context.Context, identity.Principal, string, int64, bool) (bots.Bot, error)
	GetAvatar(context.Context, identity.Principal, string) (bots.Avatar, error)
	PutAvatar(context.Context, identity.Principal, string, int64, []byte) (bots.Bot, error)
	RemoveAvatar(context.Context, identity.Principal, string, int64) (bots.Bot, error)
	GetGroup(context.Context, identity.Principal, string) (bots.Group, error)
	ListGroups(context.Context, identity.Principal, string, int, bool) ([]bots.Group, error)
	CreateGroup(context.Context, identity.Principal, bots.CreateGroupRequest) (bots.Group, bool, error)
	UpdateGroup(context.Context, identity.Principal, string, bots.UpdateGroupRequest) (bots.Group, error)
	SetGroupArchived(context.Context, identity.Principal, string, int64, bool) (bots.Group, error)
	Snapshot(context.Context, identity.Principal, int) (bots.Snapshot, error)
	UpdateWorkspace(context.Context, identity.Principal, bots.WorkspaceRequest) (bots.Workspace, error)
	GetConversation(context.Context, identity.Principal, string) (bots.Conversation, error)
	ListConversations(context.Context, identity.Principal, string, int, bool) ([]bots.Conversation, error)
}

var _ Catalog = (*bots.Service)(nil)

// Execution 只暴露用户消息和运行查询，不把调度租约、模型绑定或操作写入权交给客户端。
type Execution interface {
	Send(context.Context, identity.Principal, string, execution.SendRequest) (execution.Submission, error)
	Messages(context.Context, identity.Principal, string, int64, int) ([]execution.Message, error)
	MarkRead(context.Context, identity.Principal, string, int64) error
	GetRun(context.Context, identity.Principal, string) (execution.Run, error)
	Cancel(context.Context, identity.Principal, string) (execution.Run, error)
}

var _ Execution = (*execution.Service)(nil)

type Handler struct {
	catalog  Catalog
	executor Execution
	logger   *slog.Logger
	router   http.Handler
}
type requestKey struct{}
type requestIdentity struct {
	principal identity.Principal
	attempt   identity.Attempt
}

// New 将所有路由置于桌面认证范围，管理 Cookie 不能直接读取用户内容。
func New(catalog Catalog, executor Execution, gate *authapi.Handler, logger *slog.Logger) (*Handler, error) {
	if catalog == nil || executor == nil || gate == nil || logger == nil {
		return nil, errors.New("用户 HTTP 依赖无效")
	}
	handler := &Handler{catalog: catalog, executor: executor, logger: logger}
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, 404, "NOT_FOUND", "接口不存在")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, 405, "METHOD_NOT_ALLOWED", "请求方式不支持")
	})
	router.Get("/api/v2/workspace", handler.snapshot)
	router.Put("/api/v2/workspace", handler.updateWorkspace)
	router.Get("/api/v2/bots", handler.listBots)
	router.Post("/api/v2/bots", handler.createBot)
	router.Get("/api/v2/bots/{botID}", handler.getBot)
	router.Put("/api/v2/bots/{botID}", handler.updateBot)
	router.Post("/api/v2/bots/{botID}/state", handler.botState)
	router.Get("/api/v2/bots/{botID}/avatar", handler.getAvatar)
	router.Put("/api/v2/bots/{botID}/avatar", handler.putAvatar)
	router.Delete("/api/v2/bots/{botID}/avatar", handler.removeAvatar)
	router.Get("/api/v2/groups", handler.listGroups)
	router.Post("/api/v2/groups", handler.createGroup)
	router.Get("/api/v2/groups/{groupID}", handler.getGroup)
	router.Put("/api/v2/groups/{groupID}", handler.updateGroup)
	router.Post("/api/v2/groups/{groupID}/state", handler.groupState)
	router.Get("/api/v2/conversations", handler.listConversations)
	router.Get("/api/v2/conversations/{conversationID}", handler.getConversation)
	router.Get("/api/v2/conversations/{conversationID}/messages", handler.listMessages)
	router.Post("/api/v2/conversations/{conversationID}/messages", handler.sendMessage)
	router.Post("/api/v2/conversations/{conversationID}/read", handler.markRead)
	router.Get("/api/v2/runs/{runID}", handler.getRun)
	router.Post("/api/v2/runs/{runID}/cancel", handler.cancelRun)
	protected, err := gate.Protect("desktop", func(w http.ResponseWriter, r *http.Request, principal identity.Principal, attempt identity.Attempt) {
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, requestIdentity{principal, attempt}))
		messagesPath := strings.HasPrefix(r.URL.Path, "/api/v2/conversations/") && strings.HasSuffix(r.URL.Path, "/messages")
		allowsQuery := r.Method == "GET" && (r.URL.Path == "/api/v2/bots" || r.URL.Path == "/api/v2/groups" || r.URL.Path == "/api/v2/conversations" || r.URL.Path == "/api/v2/workspace" || messagesPath)
		if r.URL.RawQuery != "" && !allowsQuery {
			handler.failure(w, r, 400, "INPUT_INVALID", "此接口不接受查询参数")
			return
		}
		router.ServeHTTP(w, r)
	})
	if err != nil {
		return nil, err
	}
	handler.router = protected
	return handler, nil
}

// ServeHTTP 提供新的用户资源协议；宿主 HTTP 服务须设置读写及空闲时限。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler.router.ServeHTTP(w, r)
}
func current(r *http.Request) requestIdentity {
	return r.Context().Value(requestKey{}).(requestIdentity)
}

func (handler *Handler) decode(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) bool {
	if failure := httpjson.Decode(r, target, maxBytes); failure != nil {
		handler.failure(w, r, failure.Status, failure.Code, failure.Message)
		return false
	}
	return true
}
func (handler *Handler) respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err = w.Write(encoded); err != nil {
		handler.logger.WarnContext(r.Context(), "写入用户接口响应失败", "request_id", current(r).attempt.RequestID, "user_id", current(r).principal.UserID)
	}
}
func (handler *Handler) failure(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	handler.respond(w, r, status, struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}{code, message, current(r).attempt.RequestID})
}
func (handler *Handler) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 503, "USER_SERVICE_UNAVAILABLE", "用户资源服务暂不可用"
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		status, code, message = 401, "SESSION_INVALID", identity.ErrInvalidSession.Error()
	case errors.Is(err, bots.ErrInvalidInput):
		status, code, message = 400, "INPUT_INVALID", bots.ErrInvalidInput.Error()
	case errors.Is(err, bots.ErrNotFound):
		status, code, message = 404, "RESOURCE_NOT_FOUND", bots.ErrNotFound.Error()
	case errors.Is(err, bots.ErrConflict):
		status, code, message = 409, "RESOURCE_CONFLICT", bots.ErrConflict.Error()
	case errors.Is(err, bots.ErrCapacity):
		status, code, message = 409, "RESOURCE_CAPACITY", bots.ErrCapacity.Error()
	case errors.Is(err, bots.ErrArchived):
		status, code, message = 409, "RESOURCE_ARCHIVED", bots.ErrArchived.Error()
	case errors.Is(err, bots.ErrInviteDenied):
		status, code, message = 403, "GROUP_INVITE_DENIED", bots.ErrInviteDenied.Error()
	case errors.Is(err, execution.ErrInvalidInput):
		status, code, message = 400, "INPUT_INVALID", execution.ErrInvalidInput.Error()
	case errors.Is(err, execution.ErrNotFound):
		status, code, message = 404, "RESOURCE_NOT_FOUND", execution.ErrNotFound.Error()
	case errors.Is(err, execution.ErrConflict):
		status, code, message = 409, "EXECUTION_CONFLICT", execution.ErrConflict.Error()
	case errors.Is(err, execution.ErrBusy):
		status, code, message = 409, "CONVERSATION_BUSY", execution.ErrBusy.Error()
	case errors.Is(err, execution.ErrCapacity):
		status, code, message = 409, "QUEUE_CAPACITY", execution.ErrCapacity.Error()
	case errors.Is(err, execution.ErrUnavailable):
		status, code, message = 409, "EXECUTION_UNAVAILABLE", execution.ErrUnavailable.Error()
	case errors.Is(err, execution.ErrConversationKind):
		status, code, message = 409, "CONVERSATION_KIND_UNAVAILABLE", execution.ErrConversationKind.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, "REQUEST_TIMEOUT", "请求超时，请重新读取资源状态"
	default:
		handler.logger.ErrorContext(r.Context(), "用户资源操作失败", "request_id", current(r).attempt.RequestID, "user_id", current(r).principal.UserID)
	}
	handler.failure(w, r, status, code, message)
}

func (handler *Handler) page(w http.ResponseWriter, r *http.Request, workspace bool) (string, int, bool, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.serviceError(w, r, bots.ErrInvalidInput)
		return "", 0, false, false
	}
	for key, values := range query {
		if len(values) != 1 || key != "limit" && (workspace || key != "before" && key != "archived") {
			handler.serviceError(w, r, bots.ErrInvalidInput)
			return "", 0, false, false
		}
	}
	limit := 50
	if value, exists := query["limit"]; exists {
		limit, err = strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > 100 {
			handler.serviceError(w, r, bots.ErrInvalidInput)
			return "", 0, false, false
		}
	}
	archived := false
	if value, exists := query["archived"]; exists {
		if value[0] != "true" && value[0] != "false" {
			handler.serviceError(w, r, bots.ErrInvalidInput)
			return "", 0, false, false
		}
		archived = value[0] == "true"
	}
	return query.Get("before"), limit, archived, true
}
func (handler *Handler) result(w http.ResponseWriter, r *http.Request, key string, value any, err error) {
	if err != nil {
		handler.serviceError(w, r, err)
		return
	}
	handler.respond(w, r, 200, map[string]any{key: value})
}
