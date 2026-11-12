package adminapi

import (
	"net/http"
	"strings"
	"testing"

	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

func TestAccountCatalogRoutes(t *testing.T) {
	for path, key := range map[string]string{"users": "users", "computer-nodes": "nodes", "computer-images": "images"} {
		t.Run(path, func(t *testing.T) {
			fixture := newFixture(t)
			before := ids.New()
			response := fixture.run(fixture.request("GET", "/api/v2/admin/"+path+"?before="+before+"&limit=2", ""))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"`+key+`":[]`) || fixture.accounts.before != before || fixture.accounts.limit != 2 || fixture.accounts.principal.UserID != fixture.identity.session.Principal.UserID {
				t.Fatal("目录路由或分页错误", response.Code, response.Body.String())
			}
			for _, query := range []string{"?user_id=" + ids.New(), "?limit=0", "?limit=101", "?limit=1&limit=2"} {
				calls := fixture.accounts.calls
				response = fixture.run(fixture.request("GET", "/api/v2/admin/"+path+query, ""))
				if response.Code != 400 || fixture.accounts.calls != calls {
					t.Fatal("目录参数未拒绝", query, response.Code)
				}
			}
			request := fixture.request("GET", "/api/v2/admin/"+path, "")
			request.Header.Del("Cookie")
			if response = fixture.run(request); response.Code != 401 {
				t.Fatal("未登录可读目录", response.Code)
			}
			fixture.accounts.err = identity.ErrForbidden
			if response = fixture.run(fixture.request("GET", "/api/v2/admin/"+path, "")); response.Code != 403 {
				t.Fatal("领域拒绝被忽略", response.Code)
			}
		})
	}
}
