package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestGroupHTTPParallelToolPostsAndEventStream(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := map[string]int{}
	groupID := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role      string            `json:"role"`
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, "invalid input", 400)
			return
		}
		if len(input.Messages) < 2 {
			t.Error("模型上下文缺失")
			http.Error(w, "invalid input", 400)
			return
		}
		identity := strings.Split(input.Messages[0].Content, "\n")[1]
		mu.Lock()
		calls[identity]++
		attempt := calls[identity]
		mu.Unlock()
		var delta map[string]any
		reason := "stop"
		if attempt == 1 {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			arguments, err := json.Marshal(map[string]string{"group_id": groupID, "message": "已提交公开进展，产物路径 shared/report.md"})
			if err != nil {
				t.Error(err)
				return
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "post", "type": "function", "function": map[string]string{"name": "post_to_group", "arguments": string(arguments)}}}}
			reason = "tool_calls"
		} else {
			previous, last := input.Messages[len(input.Messages)-2], input.Messages[len(input.Messages)-1]
			if previous.Role != "assistant" || len(previous.ToolCalls) != 1 || last.Role != "tool" || !strings.Contains(last.Content, `"posted":true`) {
				t.Errorf("工具协议链不完整：%+v", input.Messages)
				http.Error(w, "invalid tool sequence", 400)
				return
			}
			delta = map[string]any{"content": identity + " 已完成分工，并确认公开消息和产物路径已保存。"}
		}
		encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(upstream.Close)
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := server.Auth.Login(ctx, password, "群组流测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	provider, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "群组流测试", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000000)
	supports := true
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: provider.ID, ModelKey: "fixture", DisplayName: "协作模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, SupportsTools: &supports, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	var members []string
	for _, template := range []string{"researcher", "writer", "assistant"} {
		bot, err := server.Bots.Create(ctx, service.BotInput{Template: template, ToolPolicy: &domain.ToolPolicy{Approval: "standard", AllowedTools: []string{"post_to_group", "group_members"}}}, "user")
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, bot.ID)
	}
	group, err := server.Groups.Create(ctx, service.GroupInput{Name: "端到端协作", Members: members}, "user")
	if err != nil {
		t.Fatal(err)
	}
	groupID = group.ID
	handler := server.Handler()
	response := request(t, handler, "POST", "/api/v1/conversations/"+group.ConversationID+"/messages", agent.SendInput{Content: "请分别核验，并追加公开进展"}, headers)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	var sent agent.Submission
	if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP 模型请求未并行启动")
		}
	}
	snapshot, err := server.Conversations.Snapshot(ctx, group.ConversationID)
	if err != nil || len(snapshot.ActiveRuns) != 3 {
		t.Fatal("快照缺少并行成员", snapshot.ActiveRuns, err)
	}
	close(release)
	host := httptest.NewServer(handler)
	defer host.Close()
	streamCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "GET", host.URL+"/api/v1/events?conversation_id="+group.ConversationID, nil)
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
	scanner := bufio.NewScanner(stream.Body)
	foundPlan, finished := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "event: turn.plan" {
			foundPlan = true
		}
		if line == "event: turn.finished" {
			finished = true
			break
		}
	}
	if !foundPlan || !finished {
		t.Fatal("群 SSE 未完整交付规划与收尾", scanner.Err())
	}
	turns, err := server.Runtime.Turns(ctx, group.ConversationID, "", 10)
	if err != nil || len(turns) != 1 || turns[0].Status != "succeeded" || turns[0].CostMicro != 90 {
		t.Fatal("真实群回合结果错误", turns, err)
	}
	snapshot, err = server.Conversations.Snapshot(ctx, group.ConversationID)
	if err != nil || snapshot.ActiveTurnID != nil || len(snapshot.ActiveRuns) != 0 || len(snapshot.Messages) != 13 {
		t.Fatal("消息、追加发言或收尾快照错误", snapshot, err)
	}
	posts, replies, toolResults := 0, 0, 0
	for _, message := range snapshot.Messages {
		for _, block := range message.Blocks {
			if block.Kind == "plan" {
				t.Fatal("平等群聊不应显示分工计划")
			}
			if block.Kind == "text" && strings.HasPrefix(block.Text, "已提交公开进展") {
				posts++
			}
			if block.Kind == "text" && strings.HasPrefix(block.Text, "Name:") {
				replies++
			}
			if block.Kind == "tool_result" {
				toolResults++
			}
		}
	}
	if posts != 3 || replies != 3 || toolResults != 3 {
		t.Fatal("群消息类型或工具结果不完整", posts, replies, toolResults)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatal("模型未区分成员", calls)
	}
	for _, count := range calls {
		if count != 2 {
			t.Fatal("工具调用被重放", calls)
		}
	}
}

func TestGroupHTTPAuthenticationCRUDAndRevision(t *testing.T) {
	server, code := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, code); err != nil {
		t.Fatal(err)
	}
	session, err := server.Auth.Login(context.Background(), password, "群组测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	handler := server.Handler()
	if response := request(t, handler, "GET", "/api/v1/groups", nil, nil); response.Code != 401 {
		t.Fatal("群资料缺少鉴权")
	}
	var bots []domain.Bot
	for _, template := range []string{"researcher", "writer"} {
		bot, err := server.Bots.Create(context.Background(), service.BotInput{Template: template}, "user")
		if err != nil {
			t.Fatal(err)
		}
		bots = append(bots, bot)
	}
	response := request(t, handler, "POST", "/api/v1/groups", service.GroupInput{Name: "接口测试", Members: []string{bots[0].ID}}, headers)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var group domain.Group
	if err = json.Unmarshal(response.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/groups/" + group.ID
	response = request(t, handler, "POST", path+"/members", service.GroupMembersInput{Revision: 1, BotIDs: []string{bots[1].ID}}, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "PATCH", path, map[string]any{"revision": 1, "name": "过期编辑"}, headers)
	if response.Code != 409 {
		t.Fatal("旧版本未被拒绝", response.Code)
	}
	response = request(t, handler, "PATCH", path, map[string]any{"revision": 2, "name": "平等协作"}, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "GET", path, nil, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if err = json.Unmarshal(response.Body.Bytes(), &group); err != nil || group.Revision != 3 || len(group.Members) != 2 || group.Name != "平等协作" {
		t.Fatal(group, err)
	}
	response = request(t, handler, "DELETE", path+"/members/"+bots[1].ID+"?revision=3", nil, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "DELETE", path, nil, headers)
	if response.Code != 400 {
		t.Fatal("删除未要求版本号")
	}
	response = request(t, handler, "DELETE", path+fmt.Sprintf("?revision=%d", 4), nil, headers)
	if response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request(t, handler, "GET", path, nil, headers)
	if response.Code != 404 {
		t.Fatal("已删群仍可读取")
	}
}
