package httpapi

import (
	"testing"

	"io.yueyang/tom/internal/testkit"
)

func TestAuditHTTPIsRemoved(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(t.Context(), password, "审计已移除", "linux", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		if response := request(t, server.Handler(), "GET", path, nil, headers); response.Code != 404 {
			t.Fatalf("审计接口应该已经删除: %s %d", path, response.Code)
		}
	}
}
