package httpapi

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestBootstrapMaintenanceRequiresCurrentAuthorityAndPreservesBot(t *testing.T) {
	server, code := fixture(t, true)
	handler := server.Handler()
	ctx := context.Background()
	setup := map[string]string{"X-Tom-Setup-Code": code}
	for _, action := range []string{"retry", "repair", "reset"} {
		path := "/api/v1/bootstrap/" + action
		if got := request(t, handler, "POST", path, nil, nil); got.Code != 401 {
			t.Fatal("匿名维护未被拒绝", action, got.Code)
		}
		if got := request(t, handler, "POST", path, nil, setup); got.Code != 202 {
			t.Fatal("初始化授权无法维护", action, got.Code)
		}
		server.WaitMaintenance()
		if !server.Provision.Ready() {
			t.Fatal("引导未恢复")
		}
	}
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(ctx, password, "引导验收", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	device := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if got := request(t, handler, "POST", "/api/v1/bootstrap/reset", nil, setup); got.Code != 401 {
		t.Fatal("初始化后仍接受初始化码", got.Code)
	}
	if got := request(t, handler, "POST", "/api/v1/bootstrap/reset", nil, device); got.Code != 202 {
		t.Fatal("设备重置失败", got.Code)
	}
	server.WaitMaintenance()
	if got := request(t, handler, "GET", "/api/v1/bots/"+bot.ID, nil, device); got.Code != 200 {
		t.Fatal("重置影响 Bot 或设备授权", got.Code)
	}
	if got := request(t, handler, "DELETE", "/api/v1/devices/"+session.DeviceID, nil, device); got.Code != 204 {
		t.Fatal("撤销设备失败", got.Code)
	}
	if got := request(t, handler, "POST", "/api/v1/bootstrap/repair", nil, device); got.Code != 401 {
		t.Fatal("已撤销设备可修复", got.Code)
	}
}
