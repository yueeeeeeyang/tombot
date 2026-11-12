package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/workspace"
)

func TestWorkspaceHTTPAuthWriteReadAndIsolation(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "文件测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	if response := request(t, handler, "GET", "/api/v1/fs/tree", nil, nil); response.Code != 401 {
		t.Fatal("未登录仍可读取文件")
	}
	bot, err := server.Bots.Create(context.Background(), service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	input := service.FileWriteInput{BotID: bot.ID, Scope: "bot", Path: "out/report.md", Content: "文件接口验收"}
	response := request(t, handler, "PUT", "/api/v1/fs/file", input, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "GET", "/api/v1/fs/file?bot_id="+bot.ID+"&path=out/report.md", nil, headers)
	if response.Code != 200 || !strings.Contains(response.Body.String(), input.Content) {
		t.Fatal("文件未读回")
	}
	input.BotID = ""
	input.Scope = "shared"
	input.Path = "shared/note.md"
	response = request(t, handler, "PUT", "/api/v1/fs/file", input, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "GET", "/api/v1/fs/tree", nil, headers)
	var listing workspace.Listing
	if err = json.Unmarshal(response.Body.Bytes(), &listing); err != nil || len(listing.Entries) != 1 || listing.Entries[0].Path != "shared/note.md" {
		t.Fatal("共享目录路径不一致", err)
	}
	response = request(t, handler, "GET", "/api/v1/fs/file?path="+listing.Entries[0].Path, nil, headers)
	if response.Code != 200 {
		t.Fatal("返回路径不能再次读取")
	}
	input.Scope = "bot"
	input.BotID = bot.ID
	input.Path = "../other/private.txt"
	input.CreateDirs = true
	if response = request(t, handler, "PUT", "/api/v1/fs/file", input, headers); response.Code != 400 {
		t.Fatal("允许路径越界", response.Code)
	}
	for _, query := range []string{"depth=0", "depth=17", "depth=bad"} {
		if response = request(t, handler, "GET", "/api/v1/fs/tree?"+query, nil, headers); response.Code != 400 {
			t.Fatal("无效深度被接受")
		}
	}
}
