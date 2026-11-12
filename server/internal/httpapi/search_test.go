package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestGlobalSearchHTTPAuthenticationHistoryAndMessageWindow(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(t.Context(), password, "检索测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	// 通过依赖注入准备旧历史，HTTP 仍经过真实就绪门禁、设备认证与路由注册。
	db := testkit.Database(t)
	events := bus.New(db)
	server.Conversations = service.NewConversations(db, events)
	bot, err := service.NewBots(db, events, t.TempDir()).Create(t.Context(), service.BotInput{Template: "researcher", Name: "全局检索员"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	messageID := ""
	for seq := 1; seq <= 70; seq++ {
		body := "普通近况"
		if seq == 3 {
			body = "可定位的历史档案"
		}
		encoded, err := json.Marshal([]domain.MessageBlock{{Kind: "text", Text: body}})
		if err != nil {
			t.Fatal(err)
		}
		id := testkit.Random(t)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES(?,?,?,'user',?,1000)`, id, bot.ConversationID, seq, string(encoded)); err != nil {
			t.Fatal(err)
		}
		if seq == 3 {
			messageID = id
		}
	}
	handler := server.Handler()
	for _, path := range []string{"/api/v1/search?q=" + url.QueryEscape("历史档案"), "/api/v1/conversations/" + bot.ConversationID + "/messages?around_seq=3"} {
		if response := request(t, handler, http.MethodGet, path, nil, nil); response.Code != http.StatusUnauthorized {
			t.Fatal("搜索或上下文读取绕过了认证", response.Code)
		}
	}
	response := request(t, handler, http.MethodGet, "/api/v1/search?q="+url.QueryEscape("历史档案"), nil, headers)
	var results service.SearchPage
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &results) != nil || len(results.Items) != 1 || results.Items[0].MessageID != messageID || results.Items[0].Seq != 3 {
		t.Fatal("HTTP 未返回完整历史命中位置", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		query       string
		first, last int64
	}{
		{"around_seq=3&limit=5", 1, 5},
		{"before_seq=3&limit=2", 1, 2},
		{"limit=2", 69, 70},
	} {
		response := request(t, handler, http.MethodGet, "/api/v1/conversations/"+bot.ConversationID+"/messages?"+test.query, nil, headers)
		var messages []domain.Message
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &messages) != nil || len(messages) != int(test.last-test.first+1) || messages[0].Seq != test.first || messages[len(messages)-1].Seq != test.last {
			t.Fatal("消息窗口改变了数组契约或分页顺序", test, response.Code, response.Body.String())
		}
	}
	for _, query := range []string{"", "?q=x&limit=51", "?q=x&limit=bad", "?q=x&cursor=bad", "?q=%00"} {
		if response := request(t, handler, http.MethodGet, "/api/v1/search"+query, nil, headers); response.Code != http.StatusBadRequest {
			t.Fatal("无效查询未拒绝", query, response.Code)
		}
	}
	for _, query := range []string{"around_seq=0", "around_seq=-1", "around_seq=bad", "around_seq=", "around_seq=3&before_seq=4", "around_seq=3&before_seq=", "around_seq=3&limit=201"} {
		if response := request(t, handler, http.MethodGet, "/api/v1/conversations/"+bot.ConversationID+"/messages?"+query, nil, headers); response.Code != http.StatusBadRequest {
			t.Fatal("无效窗口未拒绝", query, response.Code)
		}
	}
	if response := request(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/conversations/%s/messages?around_seq=71", bot.ConversationID), nil, headers); response.Code != http.StatusNotFound {
		t.Fatal("不存在的锚点未返回 404", response.Code)
	}
	response = request(t, handler, http.MethodGet, "/api/v1/search?q="+url.QueryEscape(bot.Name)+"&limit=1", nil, headers)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &results) != nil || results.Items[0].Kind != "bot" || results.NextCursor == "" {
		t.Fatal("名称搜索未提供下一页", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodGet, "/api/v1/search?q="+url.QueryEscape(bot.Name)+"&limit=1&cursor="+url.QueryEscape(results.NextCursor), nil, headers)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &results) != nil || len(results.Items) != 1 || results.Items[0].Kind != "conversation" {
		t.Fatal("HTTP 游标无法继续标题命中", response.Code, response.Body.String())
	}
}
