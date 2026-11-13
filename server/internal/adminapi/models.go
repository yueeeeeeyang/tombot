package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/modelregistry"
)

// Models 只暴露管理员目录动作；运行选择、明文使用和任意模型代理不注册到 HTTP。
type Models interface {
	CreateProvider(context.Context, identity.Principal, string, string, string, string, []byte) (modelregistry.Provider, error)
	ListProviders(context.Context, identity.Principal, string, int) ([]modelregistry.Provider, error)
	CreateModel(context.Context, identity.Principal, string, modelregistry.ModelRequest) (modelregistry.Model, error)
	ListModels(context.Context, identity.Principal, string, string, int) ([]modelregistry.Model, error)
	ListRemoteModels(context.Context, identity.Principal, string) ([]modelregistry.DiscoveredModel, error)
	AddCredential(context.Context, identity.Principal, string, string, []byte) (modelregistry.Credential, error)
	ListCredentials(context.Context, identity.Principal, string, int64, int) ([]modelregistry.Credential, error)
	RevokeCredential(context.Context, identity.Principal, string, string) error
	DestroyCredential(context.Context, identity.Principal, string, string) error
	CreatePolicy(context.Context, identity.Principal, string, string) (modelregistry.Policy, error)
	ListPolicies(context.Context, identity.Principal, string, int) ([]modelregistry.Policy, error)
	CreateDraft(context.Context, identity.Principal, string, modelregistry.DraftRequest) (modelregistry.Version, error)
	ListVersions(context.Context, identity.Principal, string, int64, int) ([]modelregistry.Version, error)
	GetVersion(context.Context, identity.Principal, string, int64) (modelregistry.Version, error)
	Publish(context.Context, identity.Principal, string, string, int64, int64) (modelregistry.Version, error)
	RevokeVersion(context.Context, identity.Principal, string, string, int64) error
	SetEnabled(context.Context, identity.Principal, string, string, string, int64, bool) error
	DeleteProvider(context.Context, identity.Principal, string, string, int64) error
	SetDefault(context.Context, identity.Principal, string, string, int64) error
	Assign(context.Context, identity.Principal, string, string, string, string, string) error
}

var _ Models = (*modelregistry.Service)(nil)

func (handler *Handler) registerModels(router chi.Router) {
	router.Get("/api/v2/admin/model-providers", handler.listProviders)
	router.Post("/api/v2/admin/model-providers", handler.createProvider)
	router.Get("/api/v2/admin/model-providers/{providerID}/models", handler.listModels)
	router.Post("/api/v2/admin/model-providers/{providerID}/models", handler.createModel)
	router.Get("/api/v2/admin/model-providers/{providerID}/remote-models", handler.listRemoteModels)
	router.Get("/api/v2/admin/model-providers/{providerID}/credentials", handler.listCredentials)
	router.Post("/api/v2/admin/model-providers/{providerID}/credentials", handler.addCredential)
	router.Post("/api/v2/admin/model-credentials/{credentialID}/revoke", handler.revokeCredential)
	router.Post("/api/v2/admin/model-credentials/{credentialID}/destroy", handler.destroyCredential)
	router.Get("/api/v2/admin/model-policies", handler.listPolicies)
	router.Post("/api/v2/admin/model-policies", handler.createPolicy)
	router.Get("/api/v2/admin/model-policies/{policyID}/versions", handler.listVersions)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions", handler.createDraft)
	router.Get("/api/v2/admin/model-policies/{policyID}/versions/{version}", handler.getVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions/{version}/publish", handler.publishVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/versions/{version}/revoke", handler.revokeVersion)
	router.Post("/api/v2/admin/model-policies/{policyID}/default", handler.setDefault)
	for path, kind := range map[string]string{"model-providers": "provider", "models": "model", "model-policies": "policy"} {
		router.Post("/api/v2/admin/"+path+"/{configurationID}/state", handler.setModelEnabled(kind))
	}
	router.Post("/api/v2/admin/model-providers/{providerID}/delete", handler.deleteProvider)
	router.Put("/api/v2/admin/users/{userID}/model-policy", handler.assignPolicy)
	router.Put("/api/v2/admin/users/{userID}/bots/{botID}/model-policy", handler.assignPolicy)
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
	case errors.Is(err, identity.ErrInvalidSession):
		status, code, message = 401, "SESSION_INVALID", identity.ErrInvalidSession.Error()
	case errors.Is(err, identity.ErrVerificationRequired):
		status, code, message = 403, "VERIFICATION_REQUIRED", identity.ErrVerificationRequired.Error()
	case errors.Is(err, identity.ErrForbidden):
		status, code, message = 403, "AUTH_FORBIDDEN", identity.ErrForbidden.Error()
	case errors.Is(err, modelregistry.ErrInvalidInput):
		status, code, message = 400, "INPUT_INVALID", modelregistry.ErrInvalidInput.Error()
	case errors.Is(err, modelregistry.ErrNotFound):
		status, code, message = 404, "MODEL_CONFIGURATION_NOT_FOUND", modelregistry.ErrNotFound.Error()
	case errors.Is(err, modelregistry.ErrConflict):
		status, code, message = 409, "MODEL_CONFIGURATION_CONFLICT", modelregistry.ErrConflict.Error()
	case errors.Is(err, modelregistry.ErrUnavailable):
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
func (handler *Handler) createModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelKey       string `json:"model_key"`
		DisplayName    string `json:"display_name"`
		ContextLimit   int    `json:"context_limit"`
		OutputLimit    int    `json:"output_limit"`
		SupportsTools  bool   `json:"supports_tools"`
		SupportsVision bool   `json:"supports_vision"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	value, err := handler.models.CreateModel(r.Context(), identity.principal, identity.attempt.RequestID, modelregistry.ModelRequest{ProviderID: chi.URLParam(r, "providerID"), ModelKey: request.ModelKey, DisplayName: request.DisplayName, ContextLimit: request.ContextLimit, OutputLimit: request.OutputLimit, SupportsTools: request.SupportsTools, SupportsVision: request.SupportsVision})
	handler.modelResult(w, r, 201, "model", value, err)
}

// listRemoteModels 只读取供应商当前提供的模型标识，不写入目录。
// 规格不在上游列表里，登记时仍需管理员填写上下文与输出上限。
func (handler *Handler) listRemoteModels(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		handler.failure(w, r, 400, "INPUT_INVALID", "此接口不接受查询参数", nil)
		return
	}
	models, err := handler.models.ListRemoteModels(r.Context(), current(r).principal, chi.URLParam(r, "providerID"))
	if err != nil {
		handler.modelError(w, r, err)
		return
	}
	handler.respond(w, r, 200, map[string]any{"models": models})
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
func (handler *Handler) createDraft(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelID      string               `json:"model_id"`
		CredentialID *string              `json:"credential_id"`
		OutputLimit  int                  `json:"output_limit"`
		Prices       modelregistry.Prices `json:"prices"`
	}
	if !handler.decode(w, r, &request) {
		return
	}
	identity := current(r)
	value, err := handler.models.CreateDraft(r.Context(), identity.principal, identity.attempt.RequestID, modelregistry.DraftRequest{PolicyID: chi.URLParam(r, "policyID"), ModelID: request.ModelID, CredentialID: request.CredentialID, OutputLimit: request.OutputLimit, Prices: request.Prices})
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
func (handler *Handler) publishVersion(w http.ResponseWriter, r *http.Request) {
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
	value, err := handler.models.Publish(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "policyID"), number, request.ExpectedRevision)
	handler.modelResult(w, r, 200, "version", value, err)
}
func (handler *Handler) revokeVersion(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if !handler.decode(w, r, &request) {
		return
	}
	number, ok := handler.versionNumber(w, r, chi.URLParam(r, "version"), false)
	if !ok {
		return
	}
	identity := current(r)
	err := handler.models.RevokeVersion(r.Context(), identity.principal, identity.attempt.RequestID, chi.URLParam(r, "policyID"), number)
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
