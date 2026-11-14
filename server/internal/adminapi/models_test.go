package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
)

type modelsStub struct {
	err              error
	calls            int
	method           string
	actor            identity.Principal
	requestID        string
	arguments        []any
	borrowed         []byte
	credentialDigest [32]byte
}

func (stub *modelsStub) record(method string, actor identity.Principal, requestID string, arguments ...any) error {
	stub.calls++
	stub.method = method
	stub.actor = actor
	stub.requestID = requestID
	stub.arguments = arguments
	return stub.err
}
func (stub *modelsStub) CreateProvider(_ context.Context, a identity.Principal, r, n, p, u string, credential []byte) (modelregistry.Provider, error) {
	stub.borrowed = credential
	return modelregistry.Provider{ID: ids.New()}, stub.record("CreateProvider", a, r, n, p, u, len(credential))
}
func (stub *modelsStub) UpdateProvider(_ context.Context, a identity.Principal, r, id string, revision int64, name, protocol, baseURL string, credential []byte) (modelregistry.Provider, error) {
	stub.borrowed = credential
	return modelregistry.Provider{ID: id}, stub.record("UpdateProvider", a, r, id, revision, name, protocol, baseURL, len(credential))
}
func (stub *modelsStub) TestModel(_ context.Context, a identity.Principal, r, id string, providerRevision, modelRevision int64) (modelregistry.ModelTestResult, error) {
	return modelregistry.ModelTestResult{ModelID: id}, stub.record("TestModel", a, r, id, providerRevision, modelRevision)
}
func (stub *modelsStub) ListRemoteModels(_ context.Context, a identity.Principal, providerID string) ([]modelregistry.DiscoveredModel, error) {
	if err := stub.record("ListRemoteModels", a, "", providerID); err != nil {
		return nil, err
	}
	return []modelregistry.DiscoveredModel{{ModelKey: "remote-model", DisplayName: "远端模型"}}, nil
}
func (stub *modelsStub) ListProviders(_ context.Context, a identity.Principal, b string, l int) ([]modelregistry.Provider, error) {
	return []modelregistry.Provider{}, stub.record("ListProviders", a, "", b, l)
}
func (stub *modelsStub) GetProvider(_ context.Context, a identity.Principal, id string) (modelregistry.Provider, error) {
	return modelregistry.Provider{ID: id}, stub.record("GetProvider", a, "", id)
}
func (stub *modelsStub) CreateModel(_ context.Context, a identity.Principal, r string, v modelregistry.ModelRequest) (modelregistry.Model, error) {
	return modelregistry.Model{ID: ids.New()}, stub.record("CreateModel", a, r, v)
}
func (stub *modelsStub) ListModels(_ context.Context, a identity.Principal, p, b string, l int) ([]modelregistry.Model, error) {
	return []modelregistry.Model{}, stub.record("ListModels", a, "", p, b, l)
}
func (stub *modelsStub) AddCredential(_ context.Context, a identity.Principal, r, p string, v []byte) (modelregistry.Credential, error) {
	stub.borrowed = v
	stub.credentialDigest = sha256.Sum256(v)
	return modelregistry.Credential{ID: ids.New()}, stub.record("AddCredential", a, r, p)
}
func (stub *modelsStub) ListCredentials(_ context.Context, a identity.Principal, p string, b int64, l int) ([]modelregistry.Credential, error) {
	return []modelregistry.Credential{}, stub.record("ListCredentials", a, "", p, b, l)
}
func (stub *modelsStub) RevokeCredential(_ context.Context, a identity.Principal, r, id string) error {
	return stub.record("RevokeCredential", a, r, id)
}
func (stub *modelsStub) DestroyCredential(_ context.Context, a identity.Principal, r, id string) error {
	return stub.record("DestroyCredential", a, r, id)
}
func (stub *modelsStub) CreatePolicy(_ context.Context, a identity.Principal, r, n string) (modelregistry.Policy, error) {
	return modelregistry.Policy{ID: ids.New()}, stub.record("CreatePolicy", a, r, n)
}
func (stub *modelsStub) ListPolicies(_ context.Context, a identity.Principal, b string, l int) ([]modelregistry.Policy, error) {
	return []modelregistry.Policy{}, stub.record("ListPolicies", a, "", b, l)
}
func (stub *modelsStub) GetPolicy(_ context.Context, a identity.Principal, id string) (modelregistry.Policy, error) {
	return modelregistry.Policy{ID: id}, stub.record("GetPolicy", a, "", id)
}
func (stub *modelsStub) CreateVersion(_ context.Context, a identity.Principal, r string, v modelregistry.VersionRequest) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: v.PolicyID, Number: 1}, stub.record("CreateVersion", a, r, v)
}
func (stub *modelsStub) ListVersions(_ context.Context, a identity.Principal, id string, b int64, l int) ([]modelregistry.Version, error) {
	return []modelregistry.Version{}, stub.record("ListVersions", a, "", id, b, l)
}
func (stub *modelsStub) GetVersion(_ context.Context, a identity.Principal, id string, n int64) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: id, Number: n}, stub.record("GetVersion", a, "", id, n)
}
func (stub *modelsStub) EnableVersion(_ context.Context, a identity.Principal, r, id string, n, revision int64) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: id, Number: n, IsEnabled: true}, stub.record("EnableVersion", a, r, id, n, revision)
}
func (stub *modelsStub) DisableVersion(_ context.Context, a identity.Principal, r, id string, n, revision int64) error {
	return stub.record("DisableVersion", a, r, id, n, revision)
}
func (stub *modelsStub) SetEnabled(_ context.Context, a identity.Principal, r, k, id string, revision int64, enabled bool) error {
	return stub.record("SetEnabled", a, r, k, id, revision, enabled)
}
func (stub *modelsStub) DeleteProvider(_ context.Context, a identity.Principal, r, id string, revision int64) error {
	return stub.record("DeleteProvider", a, r, id, revision)
}
func (stub *modelsStub) UpdateModel(_ context.Context, a identity.Principal, r, id string, revision int64, request modelregistry.ModelRequest) (modelregistry.Model, error) {
	return modelregistry.Model{}, stub.record("UpdateModel", a, r, id, revision, request)
}
func (stub *modelsStub) DeleteModel(_ context.Context, a identity.Principal, r, id string, revision int64) error {
	return stub.record("DeleteModel", a, r, id, revision)
}
func (stub *modelsStub) SetDefault(_ context.Context, a identity.Principal, r, id string, revision int64) error {
	return stub.record("SetDefault", a, r, id, revision)
}
func (stub *modelsStub) Assign(_ context.Context, a identity.Principal, r, u, b, p, e string) error {
	return stub.record("Assign", a, r, u, b, p, e)
}

func TestModelManagementRoutes(t *testing.T) {
	id, bot, model := ids.New(), ids.New(), ids.New()
	for _, test := range []struct {
		method, path, body, service string
		status                      int
	}{
		{"GET", "/model-providers?limit=10", "", "ListProviders", 200},
		{"GET", "/model-providers/" + id, "", "GetProvider", 200},
		{"GET", "/model-policies/" + id, "", "GetPolicy", 200},
		{"POST", "/models/" + model + "/test", `{"expected_provider_revision":2,"expected_model_revision":3}`, "TestModel", 200},
		{"POST", "/model-providers/" + id + "/edit", `{"expected_revision":1,"name":"新名称","protocol":"responses","base_url":"https://model.example.test","credential":"test-credential"}`, "UpdateProvider", 200},
		{"POST", "/model-providers", `{"name":"测试供应商","protocol":"anthropic","base_url":"https://model.example.test","credential":"test-credential"}`, "CreateProvider", 201},
		{"GET", "/model-providers/" + id + "/models", "", "ListModels", 200},
		{"GET", "/model-providers/" + id + "/remote-models", "", "ListRemoteModels", 200},
		{"POST", "/model-providers/" + id + "/models", `{"model_key":"test-model","display_name":"测试模型","context_limit":4096,"output_limit":256,"supports_tools":true,"supports_vision":true,"reasoning_effort":"high","metadata":{"input_modalities":["text","image"],"effort":{"supported_levels":["low","high"],"default_level":"high"}}}`, "CreateModel", 201},
		{"GET", "/model-providers/" + id + "/credentials?before=4&limit=2", "", "ListCredentials", 200},
		{"POST", "/model-credentials/" + id + "/revoke", `{}`, "RevokeCredential", 204},
		{"POST", "/model-credentials/" + id + "/destroy", `{}`, "DestroyCredential", 204},
		{"GET", "/model-policies", "", "ListPolicies", 200},
		{"POST", "/model-policies", `{"name":"默认策略"}`, "CreatePolicy", 201},
		{"GET", "/model-policies/" + id + "/versions?before=3", "", "ListVersions", 200},
		{"POST", "/model-policies/" + id + "/versions", `{"routing_mode":"priority","candidates":[{"model_id":"` + model + `","priority":1,"weight":1,"output_limit":128}]}`, "CreateVersion", 201},
		{"GET", "/model-policies/" + id + "/versions/2", "", "GetVersion", 200},
		{"POST", "/model-policies/" + id + "/versions/2/enable", `{"expected_revision":7}`, "EnableVersion", 200},
		{"POST", "/model-policies/" + id + "/versions/2/disable", `{"expected_revision":7}`, "DisableVersion", 204},
		{"POST", "/model-policies/" + id + "/default", `{"expected_revision":7}`, "SetDefault", 204},
		{"POST", "/models/" + id + "/state", `{"expected_revision":7,"is_enabled":false}`, "SetEnabled", 204},
		{"POST", "/models/" + id + "/edit", `{"expected_revision":7,"model_key":"renamed","display_name":"修改后的模型","context_limit":8192,"output_limit":1024,"supports_tools":true,"supports_vision":false,"metadata":{}}`, "UpdateModel", 200},
		{"POST", "/models/" + id + "/delete", `{"expected_revision":7}`, "DeleteModel", 204},
		{"POST", "/model-providers/" + id + "/state", `{"expected_revision":7,"is_enabled":false}`, "SetEnabled", 204},
		{"POST", "/model-policies/" + id + "/state", `{"expected_revision":7,"is_enabled":true}`, "SetEnabled", 204},
		{"PUT", "/users/" + id + "/model-policy", `{"policy_id":"","expected_policy_id":""}`, "Assign", 204},
		{"PUT", "/users/" + id + "/bots/" + bot + "/model-policy", `{"policy_id":"","expected_policy_id":""}`, "Assign", 204},
	} {
		t.Run(test.service+test.path, func(t *testing.T) {
			fixture := newFixture(t)
			response := fixture.run(fixture.request(test.method, "/api/v2/admin"+test.path, test.body))
			requireStatus(t, response, test.status)
			if fixture.models.calls != 1 || fixture.models.method != test.service || fixture.models.actor.UserID != fixture.identity.session.Principal.UserID {
				t.Fatal("模型路由或身份绑定错误")
			}
			if test.method != "GET" && fixture.models.requestID == "" {
				t.Fatal("模型变更缺少服务端请求标识")
			}
			if (test.service == "GetProvider" || test.service == "GetPolicy") && fixture.models.arguments[0] != id {
				t.Fatal("详情标识未从路径绑定")
			}
			if test.service == "UpdateProvider" {
				if fixture.models.arguments[0] != id || fixture.models.arguments[1] != int64(1) || fixture.models.arguments[2] != "新名称" || !bytes.Equal(fixture.models.borrowed, make([]byte, len(fixture.models.borrowed))) {
					t.Fatal("编辑参数或凭据清理错误")
				}
			}
			if test.service == "TestModel" {
				if fixture.models.arguments[0] != model || fixture.models.arguments[1] != int64(2) || fixture.models.arguments[2] != int64(3) {
					t.Fatal("模型测试归属或修订号错误")
				}
			}
			if test.service == "UpdateModel" {
				request := fixture.models.arguments[2].(modelregistry.ModelRequest)
				if fixture.models.arguments[0] != id || fixture.models.arguments[1] != int64(7) || request.ProviderID != "" || request.ModelKey != "renamed" || request.OutputLimit != 1024 {
					t.Fatal("编辑模型归属、修订号或参数绑定错误")
				}
			}
			if test.service == "DeleteModel" && (fixture.models.arguments[0] != id || fixture.models.arguments[1] != int64(7)) {
				t.Fatal("删除模型归属或修订号绑定错误")
			}
			if test.service == "CreateModel" {
				request := fixture.models.arguments[0].(modelregistry.ModelRequest)
				if request.ProviderID != id || request.ModelKey != "test-model" || !request.SupportsVision || request.ReasoningEffort != "high" || request.Metadata.Effort == nil || request.Metadata.Effort.DefaultLevel != "high" {
					t.Fatal("模型参数未正确绑定")
				}
			}
			if test.service == "CreateVersion" {
				request := fixture.models.arguments[0].(modelregistry.VersionRequest)
				if request.PolicyID != id || len(request.Candidates) != 1 || request.Candidates[0].ModelID != model {
					t.Fatal("版本归属未从路径绑定")
				}
			}
			if test.status == 204 && response.Body.Len() != 0 {
				t.Fatal("无内容响应仍有正文")
			}
		})
	}
}

func TestRemoteModelsFailureClassification(t *testing.T) {
	for _, test := range []struct {
		failure error
		status  int
		code    string
	}{
		{modelgateway.ErrAddressDenied, 422, "MODEL_ADDRESS_DENIED"},
		{modelgateway.ErrDiscoveryAuthentication, 422, "MODEL_DISCOVERY_AUTHENTICATION"},
		{modelgateway.ErrDiscoveryUnsupported, 422, "MODEL_DISCOVERY_UNSUPPORTED"},
		{modelgateway.ErrDiscoveryRateLimited, 503, "MODEL_DISCOVERY_RATE_LIMITED"},
		{modelgateway.ErrProtocol, 502, "MODEL_DISCOVERY_INVALID_RESPONSE"},
		{modelgateway.ErrResponseTooLarge, 502, "MODEL_DISCOVERY_INVALID_RESPONSE"},
		{modelgateway.ErrUnavailable, 502, "MODEL_DISCOVERY_UNAVAILABLE"},
		{context.DeadlineExceeded, 504, "MODEL_DISCOVERY_TIMEOUT"},
	} {
		t.Run(test.code, func(t *testing.T) {
			fixture := newFixture(t)
			providerID := ids.New()
			privateMessage := randomValue(t)
			fixture.models.err = errors.Join(test.failure, errors.New(privateMessage))
			response := fixture.run(fixture.request("GET", "/api/v2/admin/model-providers/"+providerID+"/remote-models", ""))
			requireStatus(t, response, test.status)
			if !strings.Contains(response.Body.String(), test.code) || !strings.Contains(fixture.log.String(), test.code) || !strings.Contains(fixture.log.String(), providerID) {
				t.Fatal("错误分类或供应商诊断标识缺失")
			}
			if strings.Contains(response.Body.String()+fixture.log.String(), privateMessage) {
				t.Fatal("上游错误正文泄露")
			}
		})
	}
}

func TestModelDetailFailures(t *testing.T) {
	for _, resource := range []string{"model-providers", "model-policies"} {
		for _, failure := range []struct {
			err    error
			status int
		}{
			{identity.ErrForbidden, 403}, {modelregistry.ErrInvalidInput, 400}, {modelregistry.ErrNotFound, 404}, {errors.New("数据库不可用"), 503},
		} {
			fixture := newFixture(t)
			fixture.models.err = failure.err
			response := fixture.run(fixture.request("GET", "/api/v2/admin/"+resource+"/"+ids.New(), ""))
			requireStatus(t, response, failure.status)
		}
		fixture := newFixture(t)
		response := fixture.run(fixture.request("GET", "/api/v2/admin/"+resource+"/"+ids.New()+"?limit=1", ""))
		requireStatus(t, response, 400)
		if fixture.models.calls != 0 {
			t.Fatal("详情接口接受额外查询参数")
		}
	}
}

func TestModelManagementBoundaries(t *testing.T) {
	id := ids.New()
	for _, test := range []struct{ method, path, body string }{
		{"GET", "/model-providers?limit=0", ""}, {"GET", "/model-providers?limit=101", ""}, {"GET", "/model-providers?limit=1&limit=2", ""}, {"GET", "/model-providers?credential=hidden", ""},
		{"GET", "/model-providers?before=" + strings.Repeat("a", 27), ""}, {"GET", "/model-providers?limit=%xx", ""},
		{"GET", "/model-providers/" + id + "/credentials?before=-1", ""}, {"GET", "/model-policies/" + id + "/versions/01", ""}, {"GET", "/model-policies/" + id + "/versions/1?limit=2", ""},
		{"POST", "/model-providers?limit=2", `{}`}, {"POST", "/model-providers", `{"actor_id":"forged"}`},
		{"POST", "/model-providers/" + id + "/models", `{"provider_id":"forged"}`}, {"POST", "/model-policies/" + id + "/versions", `{"policy_id":"forged"}`},
		{"POST", "/models/" + id + "/test", `{"base_url":"https://forged.example.test"}`},
		{"POST", "/models/" + id + "/edit", `{"provider_id":"forged"}`},
		{"POST", "/models/" + id + "/edit", `{"is_enabled":true}`},
		{"POST", "/models/" + id + "/delete", `{"actor_id":"forged"}`},
		{"POST", "/model-providers/" + id + "/edit", `{"is_enabled":true}`},
		{"POST", "/models/" + id + "/state", `{"expected_revision":1}`}, {"PUT", "/users/" + id + "/model-policy", `{"policy_id":""}`},
	} {
		t.Run(test.path+test.body, func(t *testing.T) {
			fixture := newFixture(t)
			requireStatus(t, fixture.run(fixture.request(test.method, "/api/v2/admin"+test.path, test.body)), 400)
			if fixture.models.calls != 0 {
				t.Fatal("非法模型请求进入领域服务")
			}
		})
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Del("Cookie") }, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, func(r *http.Request) { r.Header.Set("Origin", "https://other.example.test") }} {
		fixture := newFixture(t)
		request := fixture.request("POST", "/api/v2/admin/model-providers", `{}`)
		change(request)
		requireStatus(t, fixture.run(request), 403)
		if fixture.models.calls != 0 {
			t.Fatal("认证失败仍访问模型目录")
		}
	}
	fixture := newFixture(t)
	for _, path := range []string{"/api/v2/admin/model-policies/" + id + "/versions/1/publish", "/api/v2/admin/model-policies/" + id + "/versions/1/activate", "/api/v2/admin/model-policies/" + id + "/versions/1/revoke", "/api/v2/model-providers", "/api/v2/admin/models/proxy", "/api/v2/admin/models/resolve", "/api/v2/admin/model-credentials/" + id} {
		requireStatus(t, fixture.run(fixture.request("POST", path, `{}`)), 404)
	}
}

func TestCredentialInputAndModelFailureRedaction(t *testing.T) {
	for _, failure := range []error{nil, errors.New(randomValue(t))} {
		fixture := newFixture(t)
		fixture.models.err = failure
		value := randomValue(t)
		response := fixture.run(fixture.request("POST", "/api/v2/admin/model-providers/"+ids.New()+"/credentials", `{"value":"`+value+`"}`))
		status := 201
		if failure != nil {
			status = 503
		}
		requireStatus(t, response, status)
		if fixture.models.credentialDigest != sha256.Sum256([]byte(value)) || !bytes.Equal(fixture.models.borrowed, make([]byte, len(value))) {
			t.Fatal("凭据输入未传递或使用后未清零")
		}
		if strings.Contains(response.Body.String(), value) || strings.Contains(fixture.log.String(), value) {
			t.Fatal("模型凭据泄露")
		}
		if failure != nil && (strings.Contains(response.Body.String(), failure.Error()) || strings.Contains(fixture.log.String(), failure.Error())) {
			t.Fatal("模型管理原始错误泄露")
		}
	}
	for _, test := range []struct {
		failure error
		status  int
	}{{modelregistry.ErrModelEnabled, 409}, {modelregistry.ErrModelReferenced, 409}, {identity.ErrInvalidSession, 401}, {identity.ErrForbidden, 403}, {modelregistry.ErrProviderEnabled, 409}, {modelregistry.ErrNewCredentialRequired, 400}, {modelregistry.ErrInvalidInput, 400}, {modelregistry.ErrNotFound, 404}, {modelregistry.ErrConflict, 409}, {modelregistry.ErrUnavailable, 409}, {modelregistry.ErrCredential, 409}, {modelregistry.ErrVerification, 422}, {context.DeadlineExceeded, 504}} {
		fixture := newFixture(t)
		fixture.models.err = test.failure
		requireStatus(t, fixture.run(fixture.request("POST", "/api/v2/admin/model-policies", `{"name":"策略"}`)), test.status)
	}
}
