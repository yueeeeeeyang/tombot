package adminapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
)

func TestNodeManagementRoutes(t *testing.T) {
	f := newFixture(t)
	nodeID := ids.New()
	f.nodes.detail = nodecatalog.NodeDetail{Node: nodecatalog.Node{ID: nodeID, Name: "测试节点", Status: "bootstrapping", Capacity: 4}, ConfigurationVersion: 3, Diagnosis: nodecatalog.NodeDiagnosis{Code: "quarantined", Message: "验收已隔离"}}
	path := "/api/v2/admin/computer-nodes/" + nodeID
	requireStatus(t, f.run(f.request("GET", path, "")), 200)
	if f.nodes.nodeID != nodeID || f.nodes.principal != f.identity.session.Principal {
		t.Fatal("节点详情未透传认证身份")
	}
	body := `{"name":"修改节点","capacity":6,"is_draining":true,"configuration_version":3}`
	requireStatus(t, f.run(f.request("PUT", path, body)), 200)
	if f.nodes.update.Name != "修改节点" || !f.nodes.update.IsDraining || f.nodes.update.Capacity != 6 || f.nodes.update.ConfigurationVersion != 3 || f.nodes.requestID == "" {
		t.Fatal("节点配置未完整透传")
	}
	requireStatus(t, f.run(f.request("POST", path+"/verification-retry", `{"fencing_token":7}`)), 200)
	if f.nodes.token != 7 {
		t.Fatal("未透传验收版本")
	}
	for _, method := range []string{"PUT", "POST"} {
		target, content := path, body
		if method == "POST" {
			target += "/verification-retry"
			content = `{"fencing_token":7}`
		}
		request := f.request(method, target, content)
		request.Header.Del("X-CSRF-Token")
		requireStatus(t, f.run(request), 403)
	}
	requireStatus(t, f.run(httptest.NewRequest("GET", "https://admin.example.test"+path, nil)), 401)
	requireStatus(t, f.run(f.request("GET", path+"?limit=10", "")), 400)
	requireStatus(t, f.run(f.request("PUT", path, `{"name":"修改节点","capacity":6,"configuration_version":3}`)), 400)
	requireStatus(t, f.run(f.request("PUT", path, `{"name":"修改节点","capacity":6,"status":"ready"}`)), 400)
	requireStatus(t, f.run(f.request("POST", path+"/verification-retry", `{"fencing_token":7,"force":true}`)), 400)
	for _, scenario := range []struct {
		err    error
		status int
	}{{identity.ErrForbidden, 403}, {identity.ErrVerificationRequired, 403}, {nodecatalog.ErrConflict, 409}, {nodecatalog.ErrNotFound, 404}, {nodecatalog.ErrInvalidInput, 400}, {errors.New("测试内部详情不能外泄"), 503}} {
		f.nodes.err = scenario.err
		response := f.run(f.request("PUT", path, body))
		requireStatus(t, response, scenario.status)
		if strings.Contains(response.Body.String(), "测试内部详情") {
			t.Fatal("泄露内部错误")
		}
	}
}
