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
func (stub *modelsStub) CreateProvider(_ context.Context, a identity.Principal, r, n, p, u string) (modelregistry.Provider, error) {
	return modelregistry.Provider{ID: ids.New()}, stub.record("CreateProvider", a, r, n, p, u)
}
func (stub *modelsStub) ListProviders(_ context.Context, a identity.Principal, b string, l int) ([]modelregistry.Provider, error) {
	return []modelregistry.Provider{}, stub.record("ListProviders", a, "", b, l)
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
func (stub *modelsStub) CreateDraft(_ context.Context, a identity.Principal, r string, v modelregistry.DraftRequest) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: v.PolicyID, Number: 1}, stub.record("CreateDraft", a, r, v)
}
func (stub *modelsStub) ListVersions(_ context.Context, a identity.Principal, id string, b int64, l int) ([]modelregistry.Version, error) {
	return []modelregistry.Version{}, stub.record("ListVersions", a, "", id, b, l)
}
func (stub *modelsStub) GetVersion(_ context.Context, a identity.Principal, id string, n int64) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: id, Number: n}, stub.record("GetVersion", a, "", id, n)
}
func (stub *modelsStub) Publish(_ context.Context, a identity.Principal, r, id string, n, revision int64) (modelregistry.Version, error) {
	return modelregistry.Version{PolicyID: id, Number: n, Status: "published"}, stub.record("Publish", a, r, id, n, revision)
}
func (stub *modelsStub) RevokeVersion(_ context.Context, a identity.Principal, r, id string, n int64) error {
	return stub.record("RevokeVersion", a, r, id, n)
}
func (stub *modelsStub) SetEnabled(_ context.Context, a identity.Principal, r, k, id string, revision int64, enabled bool) error {
	return stub.record("SetEnabled", a, r, k, id, revision, enabled)
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
		{"POST", "/model-providers", `{"name":"测试供应商","protocol":"anthropic","base_url":"https://model.example.test"}`, "CreateProvider", 201},
		{"GET", "/model-providers/" + id + "/models", "", "ListModels", 200},
		{"POST", "/model-providers/" + id + "/models", `{"model_key":"test-model","display_name":"测试模型","context_limit":4096,"output_limit":256,"supports_tools":true,"supports_vision":true}`, "CreateModel", 201},
		{"GET", "/model-providers/" + id + "/credentials?before=4&limit=2", "", "ListCredentials", 200},
		{"POST", "/model-credentials/" + id + "/revoke", `{}`, "RevokeCredential", 204},
		{"POST", "/model-credentials/" + id + "/destroy", `{}`, "DestroyCredential", 204},
		{"GET", "/model-policies", "", "ListPolicies", 200},
		{"POST", "/model-policies", `{"name":"默认策略"}`, "CreatePolicy", 201},
		{"GET", "/model-policies/" + id + "/versions?before=3", "", "ListVersions", 200},
		{"POST", "/model-policies/" + id + "/versions", `{"model_id":"` + model + `","credential_id":null,"output_limit":128,"prices":{}}`, "CreateDraft", 201},
		{"GET", "/model-policies/" + id + "/versions/2", "", "GetVersion", 200},
		{"POST", "/model-policies/" + id + "/versions/2/publish", `{"expected_revision":7}`, "Publish", 200},
		{"POST", "/model-policies/" + id + "/versions/2/revoke", `{}`, "RevokeVersion", 204},
		{"POST", "/model-policies/" + id + "/default", `{"expected_revision":7}`, "SetDefault", 204},
		{"POST", "/models/" + id + "/state", `{"expected_revision":7,"is_enabled":false}`, "SetEnabled", 204},
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
			if test.service == "CreateModel" {
				request := fixture.models.arguments[0].(modelregistry.ModelRequest)
				if request.ProviderID != id || request.ModelKey != "test-model" || !request.SupportsVision {
					t.Fatal("模型参数未正确绑定")
				}
			}
			if test.service == "CreateDraft" {
				request := fixture.models.arguments[0].(modelregistry.DraftRequest)
				if request.PolicyID != id || request.ModelID != model {
					t.Fatal("草稿归属未从路径绑定")
				}
			}
			if test.status == 204 && response.Body.Len() != 0 {
				t.Fatal("无内容响应仍有正文")
			}
		})
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
	for _, path := range []string{"/api/v2/model-providers", "/api/v2/admin/models/proxy", "/api/v2/admin/models/resolve", "/api/v2/admin/model-credentials/" + id} {
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
	}{{identity.ErrInvalidSession, 401}, {identity.ErrForbidden, 403}, {modelregistry.ErrInvalidInput, 400}, {modelregistry.ErrNotFound, 404}, {modelregistry.ErrConflict, 409}, {modelregistry.ErrUnavailable, 409}, {modelregistry.ErrCredential, 409}, {modelregistry.ErrVerification, 422}, {context.DeadlineExceeded, 504}} {
		fixture := newFixture(t)
		fixture.models.err = test.failure
		requireStatus(t, fixture.run(fixture.request("POST", "/api/v2/admin/model-policies", `{"name":"策略"}`)), test.status)
	}
}
