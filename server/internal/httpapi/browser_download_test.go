package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestBrowserHTTPDownloadSavesOwnerAndSharedFiles(t *testing.T) {
	t.Parallel()
	s, code := fixture(t, true)
	ctx := context.Background()
	password := testkit.Random(t)
	if err := s.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.Login(ctx, password, "下载接口测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	bot, err := s.Bots.Create(ctx, service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"", bot.ID} {
		tab, err := s.Browser.Open(ctx, service.BrowserOpenInput{URL: "https://example.com", BotID: owner}, nil, "user")
		if err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/browser/tabs/" + tab.ID + "/downloads"
		input := map[string]any{"url": "https://example.com/file", "revision": tab.Revision, "page_revision": tab.PageRevision}
		if response := request(t, s.Handler(), "POST", path, input, nil); response.Code != 401 {
			t.Fatal("下载缺少认证")
		}
		response := request(t, s.Handler(), "POST", path, input, headers)
		var result service.BrowserDownload
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.File.Path == "" {
			t.Fatal("下载接口未发布产物", response.Code, response.Body.String())
		}
		if strings.HasPrefix(result.File.Path, "shared/") != (owner == "") {
			t.Fatal("下载工作区归属错误")
		}
		file, err := s.Files.Download(ctx, owner, "", result.File.Path, "user")
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(file.File)
		closeErr := file.File.Close()
		if readErr != nil || closeErr != nil || string(content) != "测试下载内容" {
			t.Fatal("下载内容未保存", readErr, closeErr)
		}
		if response = request(t, s.Handler(), "POST", path, input, headers); response.Code != 409 {
			t.Fatal("重复旧版本下载被执行", response.Code)
		}
	}
}
