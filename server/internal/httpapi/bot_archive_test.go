package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestBotArchiveRestoresIdentityConversationAndAuthenticatedAvatar(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := server.Auth.Login(ctx, password, "归档测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if _, err = server.Bots.SetAvatar(ctx, bot.ID, "user", &picture); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	path := "/api/v1/bots/" + bot.ID
	if response := request(t, handler, "GET", path+"/avatar", nil, nil); response.Code != 401 {
		t.Fatal("未鉴权读取了头像")
	}
	for _, archived := range []bool{true, false} {
		response := request(t, handler, "PATCH", path, map[string]bool{"is_archived": archived}, headers)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		response = request(t, handler, "GET", "/api/v1/bots?archived=true&limit=50", nil, headers)
		var listed []domain.Bot
		if err = json.Unmarshal(response.Body.Bytes(), &listed); err != nil || (len(listed) == 1) != archived {
			t.Fatal("归档列表错误", listed, err)
		}
		response = request(t, handler, "GET", "/api/v1/conversations", nil, headers)
		if response.Code != 200 || strings.Contains(response.Body.String(), bot.ConversationID) == archived {
			t.Fatal("主会话与 Bot 归档状态不一致", response.Body.String())
		}
		response = request(t, handler, "GET", "/api/v1/conversations/"+bot.ConversationID, nil, headers)
		if response.Code != 200 {
			t.Fatal("无法读取原会话", response.Code)
		}
		response = request(t, handler, "GET", path+"/avatar", nil, headers)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("头像返回状态或缓存策略错误", response.Code)
		}
		if _, err = png.Decode(response.Body); err != nil {
			t.Fatal("归档期间头像损坏", err)
		}
	}
}
