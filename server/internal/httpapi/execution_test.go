package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestChatHTTPStreamingSnapshotAndCancellation(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	sentFirst := make(chan time.Time, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamCanceled)
		select {
		case <-releaseFirst:
		case <-r.Context().Done():
			return
		}
		sentFirst <- time.Now()
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"真实 HTTP 流\"}}]}\n\n")); err != nil {
			t.Error(err)
			return
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "聊天测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "测试", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: p.ID, ModelKey: "fixture", DisplayName: "测试模型", ContextWindow: 8192, MaxOutput: 512, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	response := request(t, handler, "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "生成一些文本"}, headers)
	if response.Code != 202 {
		t.Fatal("发送未接受", response.Code)
	}
	var sent agent.Submission
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: "重复"}, headers)
	if response.Code != 409 {
		t.Fatal("活跃回合没有返回 409")
	}
	snapshot, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
	if err != nil || snapshot.ActiveTurnID == nil {
		t.Fatal("快照未包含执行状态", err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "GET", host.URL+"/api/v1/events?conversation_id="+bot.ConversationID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", headers["Authorization"])
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	close(releaseFirst)
	scan := bufio.NewScanner(stream.Body)
	found := false
	for scan.Scan() {
		if strings.Contains(scan.Text(), "event: text.delta") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("未收到真实 SSE 增量", scan.Err())
	}
	elapsed := time.Since(<-sentFirst)
	t.Logf("本地上游首段至会话 SSE 到达耗时 %s", elapsed)
	if elapsed >= 350*time.Millisecond {
		t.Fatal("首段增量等待了批量刷新", elapsed)
	}
	response = request(t, handler, "POST", "/api/v1/turns/"+sent.TurnID+"/cancel", nil, headers)
	if response.Code != 202 {
		t.Fatal("取消未接受")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("取消未传递至真实 HTTP 上游")
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		wake := server.Bus.Wake()
		run, err := server.Runtime.Run(ctx, sent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "canceled" {
			break
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatal("取消未落库")
		}
	}
	snapshot, err = server.Conversations.Snapshot(ctx, bot.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveTurnID != nil {
		t.Fatal("终止后仍显示活跃回合")
	}
	var partial *domain.Message
	for index := range snapshot.Messages {
		if snapshot.Messages[index].SenderKind == "bot" {
			partial = &snapshot.Messages[index]
		}
	}
	if partial == nil || partial.Status != "canceled" || partial.Blocks[0].Text != "真实 HTTP 流" {
		t.Fatal("取消丢失已收到的文本")
	}
	response = request(t, handler, "GET", "/api/v1/runs/"+sent.RunID, nil, headers)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "tool_calls") {
		t.Fatal("执行详情契约不完整")
	}
}
