// Package adminapi 提供管理平台业务接口，不向管理者暴露用户聊天或文件内容。
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/accounts"
	"io.yueyang/tom/internal/authapi"
	"io.yueyang/tom/internal/httpjson"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/nodecatalog"
)

// Accounts 仅包含平台管理动作；本地首位管理员引导方法不会注册到 HTTP。
type Accounts interface {
	ListUsers(context.Context, identity.Principal, string, int) ([]accounts.UserSummary, error)
	ListNodes(context.Context, identity.Principal, string, int) ([]accounts.NodeSummary, error)
	ListImages(context.Context, identity.Principal, string, int) ([]accounts.ImageSummary, error)
	Create(context.Context, identity.Principal, accounts.CreateRequest) (accounts.Operation, error)
	Get(context.Context, identity.Principal, string) (accounts.Operation, error)
	Retry(context.Context, identity.Principal, string, string) (accounts.Operation, error)
	Reconcile(context.Context, identity.Principal, string, string) (accounts.Operation, error)
	IssueActivation(context.Context, identity.Principal, string, string) (accounts.Activation, error)
}

// Administrators 是管理员账号管理接口；管理员账号不进入用户开户流程，也不持有电脑。
// 创建时由调用方设定初始口令并一次性返回动态验证器密钥，不再需要激活材料。
type Administrators interface {
	ListAdministrators(context.Context, identity.Principal, string, int) ([]identity.Administrator, error)
	CreateAdministrator(context.Context, identity.Principal, string, string, string, identity.Attempt) (identity.AdministratorSetup, error)
}

// Nodes 是节点目录配置接口；节点连接入口仍只来自部署配置，不接受页面输入的地址。
type Nodes interface {
	Register(context.Context, identity.Principal, nodecatalog.Registration, string) (nodecatalog.Catalog, error)
	Get(context.Context, identity.Principal, string) (nodecatalog.Catalog, error)
}

// Handler 必须由认证门保护全部路径，包括未匹配和方法错误响应。
type Handler struct {
	accounts       Accounts
	administrators Administrators
	nodes          Nodes
	models         Models
	logger         *slog.Logger
	router         http.Handler
}

type requestKey struct{}
type requestIdentity struct {
	principal identity.Principal
	attempt   identity.Attempt
}

// New 复用认证模块的管理 Cookie、来源和 CSRF 校验，避免建立第二套身份规则。
func New(service Accounts, administratorsService Administrators, nodes Nodes, models Models, gate *authapi.Handler, logger *slog.Logger) (*Handler, error) {
	if service == nil || administratorsService == nil || nodes == nil || models == nil || gate == nil || logger == nil {
		return nil, errors.New("管理 HTTP 依赖配置无效")
	}
	handler := &Handler{accounts: service, administrators: administratorsService, nodes: nodes, models: models, logger: logger}
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, 404, "NOT_FOUND", "接口不存在", nil)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		handler.failure(w, r, 405, "METHOD_NOT_ALLOWED", "请求方式不支持", nil)
	})
	router.Post("/api/v2/admin/users", handler.create)
	router.Get("/api/v2/admin/users", catalogHandler(handler, "users", service.ListUsers))
	router.Get("/api/v2/admin/computer-nodes", catalogHandler(handler, "nodes", service.ListNodes))
	router.Get("/api/v2/admin/computer-images", catalogHandler(handler, "images", service.ListImages))
	router.Post("/api/v2/admin/users/{userID}/activation", handler.activation)
	router.Get("/api/v2/admin/administrators", catalogHandler(handler, "administrators", administratorsService.ListAdministrators))
	router.Post("/api/v2/admin/administrators", handler.createAdministrator)
	router.Post("/api/v2/admin/computer-nodes", handler.registerNode)
	router.Get("/api/v2/admin/computer-nodes/{imageID}", handler.nodeCatalog)
	router.Get("/api/v2/admin/user-creation-operations/{operationID}", handler.operation)
	router.Post("/api/v2/admin/user-creation-operations/{operationID}/retry", handler.retry)
	router.Post("/api/v2/admin/user-creation-operations/{operationID}/reconcile", handler.reconcile)
	handler.registerModels(router)
	protected, err := gate.Protect("admin", func(w http.ResponseWriter, r *http.Request, principal identity.Principal, attempt identity.Attempt) {
		if r.URL.RawQuery != "" && !isModelListRequest(r) && !isAccountListRequest(r) {
			r = r.WithContext(context.WithValue(r.Context(), requestKey{}, requestIdentity{principal, attempt}))
			handler.failure(w, r, 400, "INPUT_INVALID", "此接口不接受查询参数", nil)
			return
		}
		router.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, requestIdentity{principal, attempt})))
	})
	if err != nil {
		return nil, err
	}
	handler.router = protected
	return handler, nil
}

func isAccountListRequest(r *http.Request) bool {
	return r.Method == "GET" && (r.URL.Path == "/api/v2/admin/users" || r.URL.Path == "/api/v2/admin/computer-nodes" || r.URL.Path == "/api/v2/admin/computer-images" || r.URL.Path == "/api/v2/admin/administrators" || strings.HasPrefix(r.URL.Path, "/api/v2/admin/computer-nodes/"))
}

func catalogHandler[T any](handler *Handler, key string, list func(context.Context, identity.Principal, string, int) ([]T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		before, limit, ok := handler.listPage(w, r)
		if !ok {
			return
		}
		values, err := list(r.Context(), current(r).principal, before, limit)
		if err != nil {
			handler.serviceError(w, r, err, accounts.Operation{})
			return
		}
		handler.respond(w, r, http.StatusOK, map[string]any{key: values})
	}
}

// ServeHTTP 仅提供新管理协议；服务入口须同时配置 HTTP 读取、写入与空闲时限。
func (handler *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler.router.ServeHTTP(w, r)
}

func current(r *http.Request) requestIdentity {
	return r.Context().Value(requestKey{}).(requestIdentity)
}

func (handler *Handler) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if failure := httpjson.Decode(r, target, 16<<10); failure != nil {
		handler.failure(w, r, failure.Status, failure.Code, failure.Message, nil)
		return false
	}
	return true
}

func (handler *Handler) create(w http.ResponseWriter, r *http.Request) {
	var request accounts.CreateRequest
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	request.RequestID = identity.attempt.RequestID
	operation, err := handler.accounts.Create(r.Context(), identity.principal, request)
	if err != nil {
		handler.serviceError(w, r, err, operation)
		return
	}
	if operation.Status != "succeeded" {
		handler.serviceError(w, r, errors.New("开户服务返回未完成操作"), operation)
		return
	}
	w.Header().Set("Location", "/api/v2/admin/user-creation-operations/"+operation.ID)
	handler.respond(w, r, http.StatusCreated, struct {
		Operation accounts.Operation `json:"operation"`
	}{operation})
}

func (handler *Handler) operation(w http.ResponseWriter, r *http.Request) {
	operation, err := handler.accounts.Get(r.Context(), current(r).principal, chi.URLParam(r, "operationID"))
	if err != nil {
		handler.serviceError(w, r, err, operation)
		return
	}
	handler.respond(w, r, 200, struct {
		Operation accounts.Operation `json:"operation"`
	}{operation})
}

func (handler *Handler) retry(w http.ResponseWriter, r *http.Request) {
	handler.mutateOperation(w, r, handler.accounts.Retry, "succeeded")
}

func (handler *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	handler.mutateOperation(w, r, handler.accounts.Reconcile, "failed")
}

func (handler *Handler) mutateOperation(w http.ResponseWriter, r *http.Request, change func(context.Context, identity.Principal, string, string) (accounts.Operation, error), expected string) {
	var request struct{}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	operation, err := change(r.Context(), identity.principal, chi.URLParam(r, "operationID"), identity.attempt.RequestID)
	if err != nil {
		handler.serviceError(w, r, err, operation)
		return
	}
	if operation.Status != expected {
		handler.serviceError(w, r, errors.New("开户服务返回意外操作状态"), operation)
		return
	}
	handler.respond(w, r, 200, struct {
		Operation accounts.Operation `json:"operation"`
	}{operation})
}

func (handler *Handler) activation(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	activation, err := handler.accounts.IssueActivation(r.Context(), identity.principal, chi.URLParam(r, "userID"), identity.attempt.RequestID)
	if err != nil {
		handler.serviceError(w, r, err, accounts.Operation{})
		return
	}
	handler.respond(w, r, 201, activation)
}

// createAdministrator 一次返回动态验证器密钥；该值只出现这一次，服务端不保存明文。
func (handler *Handler) createAdministrator(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	setup, err := handler.administrators.CreateAdministrator(r.Context(), actor.principal, request.Username, request.DisplayName, request.Password, actor.attempt)
	if err != nil {
		handler.administratorError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v2/admin/administrators/"+setup.AdministratorID)
	handler.respond(w, r, http.StatusCreated, setup)
}

// registerNode 只登记目录元数据；节点是否可用仍由心跳、部署检查与真实能力验收决定。
func (handler *Handler) registerNode(w http.ResponseWriter, r *http.Request) {
	var registration nodecatalog.Registration
	if !handler.decode(w, r, &registration) {
		return
	}
	identity := current(r)
	catalog, err := handler.nodes.Register(r.Context(), identity.principal, registration, identity.attempt.RequestID)
	if err != nil {
		handler.nodeError(w, r, err)
		return
	}
	handler.respond(w, r, http.StatusCreated, catalog)
}

func (handler *Handler) nodeCatalog(w http.ResponseWriter, r *http.Request) {
	catalog, err := handler.nodes.Get(r.Context(), current(r).principal, chi.URLParam(r, "imageID"))
	if err != nil {
		handler.nodeError(w, r, err)
		return
	}
	handler.respond(w, r, http.StatusOK, catalog)
}

func (handler *Handler) nodeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		handler.failure(w, r, 401, "SESSION_INVALID", identity.ErrInvalidSession.Error(), nil)
	case errors.Is(err, identity.ErrVerificationRequired):
		handler.failure(w, r, 403, "VERIFICATION_REQUIRED", identity.ErrVerificationRequired.Error(), nil)
	case errors.Is(err, identity.ErrForbidden):
		handler.failure(w, r, 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error(), nil)
	case errors.Is(err, nodecatalog.ErrInvalidInput):
		handler.failure(w, r, 400, "INPUT_INVALID", nodecatalog.ErrInvalidInput.Error(), nil)
	case errors.Is(err, nodecatalog.ErrConflict):
		handler.failure(w, r, 409, "NODE_CONFLICT", nodecatalog.ErrConflict.Error(), nil)
	case errors.Is(err, nodecatalog.ErrNotFound):
		handler.failure(w, r, 404, "NODE_NOT_FOUND", nodecatalog.ErrNotFound.Error(), nil)
	default:
		handler.logger.ErrorContext(r.Context(), "节点目录服务调用失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID)
		handler.failure(w, r, 503, "NODE_CATALOG_UNAVAILABLE", "节点目录服务暂不可用，请稍后重试", nil)
	}
}

func (handler *Handler) administratorError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		handler.failure(w, r, 401, "SESSION_INVALID", identity.ErrInvalidSession.Error(), nil)
	case errors.Is(err, identity.ErrVerificationRequired):
		handler.failure(w, r, 403, "VERIFICATION_REQUIRED", identity.ErrVerificationRequired.Error(), nil)
	case errors.Is(err, identity.ErrForbidden):
		handler.failure(w, r, 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error(), nil)
	case errors.Is(err, identity.ErrInvalidInput):
		handler.failure(w, r, 400, "INPUT_INVALID", identity.ErrInvalidInput.Error(), nil)
	case errors.Is(err, identity.ErrAdministratorExists):
		handler.failure(w, r, 409, "ADMINISTRATOR_CONFLICT", identity.ErrAdministratorExists.Error(), nil)
	default:
		handler.logger.ErrorContext(r.Context(), "管理员账号服务调用失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID)
		handler.failure(w, r, 503, "ADMINISTRATORS_UNAVAILABLE", "管理员账号服务暂不可用，请稍后重试", nil)
	}
}

func (handler *Handler) serviceError(w http.ResponseWriter, r *http.Request, err error, operation accounts.Operation) {
	var details *accounts.Operation
	if operation.ID != "" {
		details = &operation
	}
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		handler.failure(w, r, 401, "SESSION_INVALID", identity.ErrInvalidSession.Error(), nil)
	case errors.Is(err, identity.ErrVerificationRequired):
		handler.failure(w, r, 403, "VERIFICATION_REQUIRED", identity.ErrVerificationRequired.Error(), nil)
	case errors.Is(err, identity.ErrForbidden):
		handler.failure(w, r, 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error(), nil)
	case errors.Is(err, accounts.ErrInvalidInput):
		handler.failure(w, r, 400, "INPUT_INVALID", accounts.ErrInvalidInput.Error(), nil)
	case errors.Is(err, accounts.ErrConflict):
		handler.failure(w, r, 409, "CREATE_CONFLICT", accounts.ErrConflict.Error(), nil)
	case errors.Is(err, accounts.ErrNoCapacity):
		handler.failure(w, r, 503, "CAPACITY_UNAVAILABLE", accounts.ErrNoCapacity.Error(), nil)
	case errors.Is(err, accounts.ErrImageUnavailable):
		handler.failure(w, r, 409, "IMAGE_UNAVAILABLE", accounts.ErrImageUnavailable.Error(), nil)
	case errors.Is(err, accounts.ErrNotFound):
		handler.failure(w, r, 404, "OPERATION_NOT_FOUND", accounts.ErrNotFound.Error(), nil)
	case errors.Is(err, accounts.ErrActivationUnavailable):
		handler.failure(w, r, 409, "ACTIVATION_UNAVAILABLE", accounts.ErrActivationUnavailable.Error(), nil)
	case errors.Is(err, accounts.ErrPending):
		handler.failure(w, r, 504, "CREATE_PENDING", accounts.ErrPending.Error(), details)
	case errors.Is(err, accounts.ErrProvisionFailed):
		handler.failure(w, r, 502, "PROVISION_FAILED", accounts.ErrProvisionFailed.Error(), details)
	case errors.Is(err, accounts.ErrQuarantined):
		handler.failure(w, r, 409, "COMPUTER_QUARANTINED", accounts.ErrQuarantined.Error(), details)
	default:
		handler.logger.ErrorContext(r.Context(), "用户管理服务调用失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID, "operation_id", operation.ID)
		handler.failure(w, r, 503, "ACCOUNTS_UNAVAILABLE", "用户管理服务暂不可用，请查询已有操作后重试", details)
	}
}

func (handler *Handler) failure(w http.ResponseWriter, r *http.Request, status int, code, message string, operation *accounts.Operation) {
	handler.respond(w, r, status, struct {
		Code      string              `json:"code"`
		Message   string              `json:"message"`
		RequestID string              `json:"request_id"`
		Operation *accounts.Operation `json:"operation,omitempty"`
	}{code, message, current(r).attempt.RequestID, operation})
}

func (handler *Handler) respond(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		handler.logger.WarnContext(r.Context(), "管理响应写入失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID)
	}
}
