package httpapi

import (
	"testing"

	"io.yueyang/tom/internal/testkit"
)

func TestUsageHTTPIsRemoved(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(t.Context(), password, "用量已移除", "linux", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	if response := request(t, server.Handler(), "GET", "/api/v1/usage", nil, headers); response.Code != 404 {
		t.Fatalf("用量接口应该已经删除: %d", response.Code)
	}
}
