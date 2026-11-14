package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
)

// Models 只暴露管理员目录动作；运行选择、明文使用和任意模型代理不注册到 HTTP。
type Models interface {
	CreateProvider(context.Context, identity.Principal, string, string, string, string, []byte) (modelregistry.Provider, error)
	UpdateProvider(context.Context, identity.Principal, string, string, int64, string, string, string, []byte) (modelregistry.Provider, error)
	ListProviders(context.Context, identity.Principal, string, int) ([]modelregistry.Provider, error)
	GetProvider(context.Context, identity.Principal, string) (modelregistry.Provider, error)
	CreateModel(context.Context, identity.Principal, string, modelregistry.ModelRequest) (modelregistry.Model, error)
	UpdateModel(context.Context, identity.Principal, string, string, int64, modelregistry.ModelRequest) (modelregistry.Model, error)
	DeleteModel(context.Context, identity.Principal, string, string, int64) error
	ListModels(context.Context, identity.Principal, string, string, int) ([]modelregistry.Model, error)
	TestModel(context.Context, identity.Principal, string, string, int64, int64) (modelregistry.ModelTestResult, error)
	ListRemoteModels(context.Context, identity.Principal, string) ([]modelregistry.DiscoveredModel, error)
	AddCredential(context.Context, identity.Principal, string, string, []byte) (modelregistry.Credential, error)
	ListCredentials(context.Context, identity.Principal, string, int64, int) ([]modelregistry.Credential, error)
	RevokeCredential(context.Context, identity.Principal, string, string) error
	DestroyCredential(context.Context, identity.Principal, string, string) error
	CreatePolicy(context.Context, identity.Principal, string, string) (modelregistry.Policy, error)
	ListPolicies(context.Context, identity.Principal, string, int) ([]modelregistry.Policy, error)
	GetPolicy(context.Context, identity.Principal, string) (modelregistry.Policy, error)
	CreateVersion(context.Context, identity.Principal, string, modelregistry.VersionRequest) (modelregistry.Version, error)
	ListVersions(context.Context, identity.Principal, string, int64, int) ([]modelregistry.Version, error)
	GetVersion(context.Context, identity.Principal, string, int64) (modelregistry.Version, error)
	EnableVersion(context.Context, identity.Principal, string, string, int64, int64) (modelregistry.Version, error)
	DisableVersion(context.Context, identity.Principal, string, string, int64, int64) error
	SetEnabled(context.Context, identity.Principal, string, string, string, int64, bool) error
	DeleteProvider(context.Context, identity.Principal, string, string, int64) error
	SetDefault(context.Context, identity.Principal, string, string, int64) error
	Assign(context.Context, identity.Principal, string, string, string, string, string) error
}

var _ Models = (*modelregistry.Service)(nil)

func (handler *Handler) registerModels(router chi.Router) {
	router.Get("/api/v2/admin/model-providers", handler.listProviders)
	router.Get("/api/v2/admin/model-providers/{providerID}", handler.getProvider)
	router.Post("/api/v2/admin/model-providers", handler.createProvider)
	router.Post("/api/v2/admin/model-providers/{providerID}/edit", handler.updateProvider)
	router.Post("/api/v2/admin/models/{modelID}/test", handler.testModel)
	router.Post("/api/v2/admin/models/{modelID}/edit", handler.updateModel)
	router.Post("/api/v2/admin/models/{modelID}/delete", handler.deleteModel)
	router.Get("/api/v2/admin/model-providers/{providerID}/models", handler.listModels)
	router.Post("/api/v2/admin/model-providers/{providerID}/models", handler.createModel)
	router.Get("/api/v2/admin/model-providers/{providerID}/remote-models", handler.listRemoteModels)
	router.Get("/api/v2/admin/model-providers/{providerID}/credentials", handler.listCredentials)
	router.Post("/api/v2/admin/model-providers/{providerID}/credentials", handler.addCredential)
	router.Post("/api/v2/admin/model-credentials/{credentialID}/revoke", handler.revokeCredential)
	router.Post("/api/v2/admin/model-credentials/{credentialID}/destroy", handler.destroyCredential)
	router.Get("/api/v2/admin/model-policies", handler.listPolicies)
	router.Get("/api/v2/admin/model-policies/{policyID}", handler.getPolicy)
	router.Post("/api/v2/admin/model-policies", handler.createPolicy)
	router.Get("/api/v2/admin/model-policies/{policyID}/versions", handler.listVersions)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions", handler.createVersion)
	router.Get("/api/v2/admin/model-policies/{policyID}/versions/{version}", handler.getVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions/{version}/enable", handler.enableVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions/{version}/disable", handler.disableVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/default", handler.setDefault)
	for path, kind := range map[string]string{"model-providers": "provider", "models": "model", "model-policies": "policy"} {
		router.Post("/api/v2/admin/"+path+"/{configurationID}/state", handler.setModelEnabled(kind))
	}
	router.Post("/api/v2/admin/model-providers/{providerID}/delete", handler.deleteProvider)
	router.Put("/api/v2/admin/users/{userID}/model-policy", handler.assignPolicy)
	router.Put("/api/v2/admin/users/{userID}/bots/{botID}/model-policy", handler.assignPolicy)
}

// updateProvider 委托领域服务校验停用状态，凭据仅在调用期间保留。
func (handler *Handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64               `json:"expected_revision"`
		Name             string              `json:"name"`
		Protocol         string              `json:"protocol"`
		BaseURL          string              `json:"base_url"`
		Credential       identity.Credential `json:"credential"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	plaintext := []byte(request.Credential)
	request.Credential = ""
	defer clear(plaintext)
	value, err := handler.models.UpdateProvider(r.Context(), actor.principal, actor.attempt.RequestID, chi.URLParam(r, "providerID"), request.ExpectedRevision, request.Name, request.Protocol, request.BaseURL, plaintext)
	handler.modelResult(w, r, 200, "provider", value, err)
}

func (handler *Handler) testModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ProviderRevision int64 `json:"expected_provider_revision"`
		ModelRevision    int64 `json:"expected_model_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	value, err := handler.models.TestModel(r.Context(), actor.principal, actor.attempt.RequestID, chi.URLParam(r, "modelID"), request.ProviderRevision, request.ModelRevision)
	if err == nil {
		log := handler.logger.InfoContext
		if value.Connectivity.Status != "succeeded" || value.Tools.Status != "succeeded" {
			log = handler.logger.WarnContext
		}
		log(r.Context(), "模型测试完成", "request_id", actor.attempt.RequestID, "actor_id", actor.principal.UserID, "model_id", value.ModelID, "connectivity", value.Connectivity.Status, "tools", value.Tools.Status)
	}
	handler.modelResult(w, r, 200, "test", value, err)
}

func isModelListRequest(r *http.Request) bool {
	if r.Method != "GET" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/admin/"), "/")
	if len(parts) == 1 {
		return parts[0] == "model-providers" || parts[0] == "model-policies"
	}
	return len(parts) == 3 && ((parts[0] == "model-providers" && (parts[2] == "models" || parts[2] == "credentials")) || (parts[0] == "model-policies" && parts[2] == "versions"))
}

func (handler *Handler) versionNumber(w http.ResponseWriter, r *http.Request, value string, allowZero bool) (int64, bool) {
	if value == "" && allowZero {
		return 0, true
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 1 || strconv.FormatInt(number, 10) != value {
		handler.modelError(w, r, modelregistry.ErrInvalidInput)
		return 0, false
	}
	return number, true
}

func (handler *Handler) modelResult(w http.ResponseWriter, r *http.Request, status int, key string, value any, err error) {
	if err != nil {
		handler.modelError(w, r, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	handler.respond(w, r, status, map[string]any{key: value})
}

func (handler *Handler) modelError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 503, "MODEL_MANAGEMENT_UNAVAILABLE", "模型管理服务暂不可用，请重新读取配置后重试"
	switch {
	case errors.Is(err, modelregistry.ErrModelEnabled):
		status, code, message = 409, "MODEL_ENABLED", modelregistry.ErrModelEnabled.Error()
	case errors.Is(err, modelregistry.ErrModelReferenced):
		status, code, message = 409, "MODEL_REFERENCED", modelregistry.ErrModelReferenced.Error()
	case errors.Is(err, identity.ErrInvalidSession):
		status, code, message = 401, "SESSION_INVALID", identity.ErrInvalidSession.Error()
	case errors.Is(err, identity.ErrVerificationRequired):
		status, code, message = 403, "VERIFICATION_REQUIRED", identity.ErrVerificationRequired.Error()
	case errors.Is(err, identity.ErrForbidden):
		status, code, message = 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error()
	case errors.Is(err, modelregistry.ErrProviderEnabled):
		status, code, message = 409, "MODEL_PROVIDER_ENABLED", modelregistry.ErrProviderEnabled.Error()
	case errors.Is(err, modelregistry.ErrNewCredentialRequired):
		status, code, message = 400, "MODEL_NEW_CREDENTIAL_REQUIRED", modelregistry.ErrNewCredentialRequired.Error()
	case errors.Is(err, modelregistry.ErrInvalidInput):
		status, code, message = 400, "INPUT_INVALID", modelregistry.ErrInvalidInput.Error()
	case errors.Is(err, modelregistry.ErrNotFound):
		status, code, message = 404, "MODEL_CONFIGURATION_NOT_FOUND", modelregistry.ErrNotFound.Error()
	case errors.Is(err, modelregistry.ErrConflict):
		status, code, message = 409, "MODEL_CONFIGURATION_CONFLICT", modelregistry.ErrConflict.Error()
	case errors.Is(err, modelregistry.ErrNoCandidate), errors.Is(err, modelregistry.ErrUnavailable):
		status, code, message = 409, "MODEL_CONFIGURATION_UNAVAILABLE", modelregistry.ErrUnavailable.Error()
	case errors.Is(err, modelregistry.ErrCredential):
		status, code, message = 409, "MODEL_CREDENTIAL_UNAVAILABLE", modelregistry.ErrCredential.Error()
	case errors.Is(err, modelregistry.ErrVerification):
		status, code, message = 422, "MODEL_VERIFICATION_FAILED", modelregistry.ErrVerification.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, "MODEL_MANAGEMENT_TIMEOUT", "模型管理请求超时，请查询当前版本状态"
	default:
		handler.logger.ErrorContext(r.Context(), "模型管理服务调用失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID)
	}
	handler.failure(w, r, status, code, message, nil)
}

// createProvider 同时登记供应商与首个凭据：只有地址没有凭据的供应商无法调用，也无从拉取模型。
func (handler *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string              `json:"name"`
		Protocol   string              `json:"protocol"`
		BaseURL    string              `json:"base_url"`
		Credential identity.Credential `json:"credential"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	plaintext := []byte(request.Credential)
	request.Credential = ""
	defer clear(plaintext)
	value, err := handler.models.CreateProvider(r.Context(), identity.principal, identity.attempt.RequestID, request.Name, request.Protocol, request.BaseURL, plaintext)
	handler.modelResult(w, r, 201, "provider", value, err)
}
func (handler *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := handler.listPage(w, r)
	if !ok {
		return
	}
	value, err := handler.models.ListProviders(r.Context(), current(r).principal, before, limit)
	handler.modelResult(w, r, 200, "providers", value, err)
}

// getProvider 读取指定供应商，业务权限与标识校验统一由领域服务处理。
func (handler *Handler) getProvider(w http.ResponseWriter, r *http.Request) {
	value, err := handler.models.GetProvider(r.Context(), current(r).principal, chi.URLParam(r, "providerID"))
	handler.modelResult(w, r, 200, "provider", value, err)
}

// getPolicy 读取指定策略，不通过目录扫描恢复页面选择。
func (handler *Handler) getPolicy(w http.ResponseWriter, r *http.Request) {
	value, err := handler.models.GetPolicy(r.Context(), current(r).principal, chi.URLParam(r, "policyID"))
	handler.modelResult(w, r, 200, "policy", value, err)
}

type modelParametersRequest struct {
	ModelKey        string                      `json:"model_key"`
	DisplayName     string                      `json:"display_name"`
	ContextLimit    int                         `json:"context_limit"`
	OutputLimit     int                         `json:"output_limit"`
	SupportsTools   bool                        `json:"supports_tools"`
	SupportsVision  bool                        `json:"supports_vision"`
	ReasoningEffort string                      `json:"reasoning_effort,omitempty"`
	Metadata        modelregistry.ModelMetadata `json:"metadata"`
}

func (handler *Handler) createModel(w http.ResponseWriter, r *http.Request) {
	var request modelParametersRequest
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	value, err := handler.models.CreateModel(r.Context(), identity.principal, identity.attempt.RequestID, modelregistry.ModelRequest{ProviderID: chi.URLParam(r, "providerID"), ModelKey: request.ModelKey, DisplayName: request.DisplayName, ContextLimit: request.ContextLimit, OutputLimit: request.OutputLimit, SupportsTools: request.SupportsTools, SupportsVision: request.SupportsVision, ReasoningEffort: request.ReasoningEffort, Metadata: request.Metadata})
	handler.modelResult(w, r, 201, "model", value, err)
}

// listRemoteModels 只读取供应商当前提供的模型标识，不写入目录。
// 保留上游明确提供的规格，缺失字段由管理员补充，不写入目录。
func (handler *Handler) listRemoteModels(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		handler.failure(w, r, 400, "INPUT_INVALID", "此接口不接受查询参数", nil)
		return
	}
	models, err := handler.models.ListRemoteModels(r.Context(), current(r).principal, chi.URLParam(r, "providerID"))
	if err != nil {
		handler.remoteModelsError(w, r, err)
		return
	}
	handler.respond(w, r, 200, map[string]any{"models": models})
}

// remoteModelsError 只返回固定分类，日志保留供应商与请求标识，不携带凭据或上游正文。
func (handler *Handler) remoteModelsError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 502, "MODEL_DISCOVERY_UNAVAILABLE", "无法连接供应商模型列表接口，请稍后重试"
	switch {
	case errors.Is(err, modelgateway.ErrAddressDenied):
		status, code, message = 422, "MODEL_ADDRESS_DENIED", "供应商地址无法访问，请使用 HTTPS 地址并检查是否命中本机、云元数据或服务端拒绝网段"
	case errors.Is(err, modelgateway.ErrDiscoveryAuthentication):
		status, code, message = 422, "MODEL_DISCOVERY_AUTHENTICATION", "供应商拒绝了访问凭据，请检查供应商凭据及模型列表读取权限"
	case errors.Is(err, modelgateway.ErrDiscoveryRateLimited):
		status, code, message = 503, "MODEL_DISCOVERY_RATE_LIMITED", "供应商请求过于频繁，请稍后重新读取模型列表"
	case errors.Is(err, modelgateway.ErrDiscoveryUnsupported):
		status, code, message = 422, "MODEL_DISCOVERY_UNSUPPORTED", "该供应商地址不支持模型列表接口，请核对 API 基础地址或手动登记模型"
	case errors.Is(err, modelgateway.ErrProtocol), errors.Is(err, modelgateway.ErrResponseTooLarge):
		code, message = "MODEL_DISCOVERY_INVALID_RESPONSE", "供应商模型列表响应格式不正确或超过大小上限，请核对协议与 API 基础地址"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = 504, "MODEL_DISCOVERY_TIMEOUT", "读取供应商模型列表超时，请稍后重试"
	case errors.Is(err, modelgateway.ErrUnavailable):
	default:
		handler.modelError(w, r, err)
		return
	}
	handler.logger.WarnContext(r.Context(), "读取供应商模型列表失败", "request_id", current(r).attempt.RequestID, "actor_id", current(r).principal.UserID, "provider_id", chi.URLParam(r, "providerID"), "error_code", code)
	handler.failure(w, r, status, code, message, nil)
}

func (handler *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := handler.listPage(w, r)
	if !ok {
		return
	}
	value, err := handler.models.ListModels(r.Context(), current(r).principal, chi.URLParam(r, "providerID"), before, limit)
	handler.modelResult(w, r, 200, "models", value, err)
}
func (handler *Handler) addCredential(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Value identity.Credential `json:"value"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	plaintext := []byte(request.Value)
	request.Value = ""
	defer clear(plaintext)
	value, err := handler.models.AddCredential(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "providerID"), plaintext)
	handler.modelResult(w, r, 201, "credential", value, err)
}
func (handler *Handler) listCredentials(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := handler.listPage(w, r)
	if !ok {
		return
	}
	number, ok := handler.versionNumber(w, r, before, true)
	if !ok {
		return
	}
	value, err := handler.models.ListCredentials(r.Context(), current(r).principal, chi.URLParam(r, "providerID"), number, limit)
	handler.modelResult(w, r, 200, "credentials", value, err)
}
func (handler *Handler) revokeCredential(w http.ResponseWriter, r *http.Request) {
	handler.changeCredential(w, r, handler.models.RevokeCredential)
}
func (handler *Handler) destroyCredential(w http.ResponseWriter, r *http.Request) {
	handler.changeCredential(w, r, handler.models.DestroyCredential)
}
func (handler *Handler) changeCredential(w http.ResponseWriter, r *http.Request, change func(context.Context, identity.Principal, string, string) error) {
	var request struct{}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	err := change(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "credentialID"))
	handler.modelResult(w, r, 204, "", nil, err)
}
func (handler *Handler) createPolicy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	value, err := handler.models.CreatePolicy(r.Context(), identity.principal, identity.attempt.RequestID, request.Name)
	handler.modelResult(w, r, 201, "policy", value, err)
}
func (handler *Handler) listPolicies(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := handler.listPage(w, r)
	if !ok {
		return
	}
	value, err := handler.models.ListPolicies(r.Context(), current(r).principal, before, limit)
	handler.modelResult(w, r, 200, "policies", value, err)
}
func (handler *Handler) createVersion(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RoutingMode string                           `json:"routing_mode"`
		Candidates  []modelregistry.CandidateRequest `json:"candidates"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	value, err := handler.models.CreateVersion(r.Context(), identity.principal, identity.attempt.RequestID, modelregistry.VersionRequest{PolicyID: chi.URLParam(r, "policyID"), RoutingMode: request.RoutingMode, Candidates: request.Candidates})
	handler.modelResult(w, r, 201, "version", value, err)
}
func (handler *Handler) listVersions(w http.ResponseWriter, r *http.Request) {
	before, limit, ok := handler.listPage(w, r)
	if !ok {
		return
	}
	number, ok := handler.versionNumber(w, r, before, true)
	if !ok {
		return
	}
	value, err := handler.models.ListVersions(r.Context(), current(r).principal, chi.URLParam(r, "policyID"), number, limit)
	handler.modelResult(w, r, 200, "versions", value, err)
}
func (handler *Handler) getVersion(w http.ResponseWriter, r *http.Request) {
	number, ok := handler.versionNumber(w, r, chi.URLParam(r, "version"), false)
	if !ok {
		return
	}
	value, err := handler.models.GetVersion(r.Context(), current(r).principal, chi.URLParam(r, "policyID"), number)
	handler.modelResult(w, r, 200, "version", value, err)
}
func (handler *Handler) enableVersion(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	number, ok := handler.versionNumber(w, r, chi.URLParam(r, "version"), false)
	if !ok {
		return
	}
	identity := current(r)
	value, err := handler.models.EnableVersion(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "policyID"), number, request.ExpectedRevision)
	handler.modelResult(w, r, 200, "version", value, err)
}
func (handler *Handler) disableVersion(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	number, ok := handler.versionNumber(w, r, chi.URLParam(r, "version"), false)
	if !ok {
		return
	}
	identity := current(r)
	err := handler.models.DisableVersion(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "policyID"), number, request.ExpectedRevision)
	handler.modelResult(w, r, 204, "", nil, err)
}
func (handler *Handler) setDefault(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	err := handler.models.SetDefault(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "policyID"), request.ExpectedRevision)
	handler.modelResult(w, r, 204, "", nil, err)
}

// deleteProvider 只删除已停用的供应商，且要求期望修订号匹配。
func (handler *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	if err := handler.models.DeleteProvider(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "providerID"), request.ExpectedRevision); err != nil {
		handler.modelError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) setModelEnabled(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ExpectedRevision int64 `json:"expected_revision"`
			IsEnabled        *bool `json:"is_enabled"`
		}
		if !handler.decode(w, r, &request) {
			return
		}
		if request.IsEnabled == nil {
			handler.modelError(w, r, modelregistry.ErrInvalidInput)
			return
		}
		identity := current(r)
		err := handler.models.SetEnabled(r.Context(), identity.principal, identity.attempt.RequestID, kind, chi.URLParam(r, "configurationID"), request.ExpectedRevision, *request.IsEnabled)
		handler.modelResult(w, r, 204, "", nil, err)
	}
}
func (handler *Handler) assignPolicy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PolicyID         string  `json:"policy_id"`
		ExpectedPolicyID *string `json:"expected_policy_id"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	if request.ExpectedPolicyID == nil {
		handler.modelError(w, r, modelregistry.ErrInvalidInput)
		return
	}
	identity := current(r)
	err := handler.models.Assign(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "userID"), chi.URLParam(r, "botID"), request.PolicyID, *request.ExpectedPolicyID)
	handler.modelResult(w, r, 204, "", nil, err)
}

// updateModel 的归属来自模型记录，接口不接受客户端改写供应商或启用状态。
func (handler *Handler) updateModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		modelParametersRequest
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	value, err := handler.models.UpdateModel(r.Context(), actor.principal, actor.attempt.RequestID, chi.URLParam(r, "modelID"), request.ExpectedRevision, modelregistry.ModelRequest{ModelKey: request.ModelKey, DisplayName: request.DisplayName, ContextLimit: request.ContextLimit, OutputLimit: request.OutputLimit, SupportsTools: request.SupportsTools, SupportsVision: request.SupportsVision, ReasoningEffort: request.ReasoningEffort, Metadata: request.Metadata})
	handler.modelResult(w, r, 200, "model", value, err)
}

func (handler *Handler) deleteModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	actor := current(r)
	err := handler.models.DeleteModel(r.Context(), actor.principal, actor.attempt.RequestID, chi.URLParam(r, "modelID"), request.ExpectedRevision)
	handler.modelResult(w, r, 204, "", nil, err)
}
