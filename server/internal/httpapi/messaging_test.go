package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestMessagingHTTPDelegationArtifactsAsyncAndSSE(t *testing.T) {
	target := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role      string            `json:"role"`
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) < 2 {
			t.Error("无效模型请求", err)
			http.Error(w, "invalid request", 400)
			return
		}
		system := input.Messages[0].Content
		last := input.Messages[len(input.Messages)-1]
		child := strings.Contains(system, "This is a one-to-one bot conversation")
		var delta map[string]any
		reason := "stop"
		if child && last.Role == "user" && strings.Contains(last.Content, "请核实共享报告的来源") {
			delta = map[string]any{"content": "共享报告的来源已核实"}
		} else if last.Role == "tool" {
			previous := input.Messages[len(input.Messages)-2]
			if previous.Role != "assistant" || len(previous.ToolCalls) != 1 {
				t.Error("交接元数据破坏了工具协议")
				http.Error(w, "invalid tool sequence", 400)
				return
			}
			if child {
				delta = map[string]any{"content": `{"summary":"共享报告已完成","artifacts":["shared/report.md"]}`}
			} else {
				if !strings.Contains(last.Content, `"delivered":true`) {
					t.Error("投递失败", last.Content)
					http.Error(w, "delivery failed", 400)
					return
				}
				if !strings.Contains(last.Content, `"summary"`) && !strings.Contains(last.Content, `"artifacts":[]`) {
					t.Error("投递结果缺失", last.Content)
				}
				delta = map[string]any{"content": "协作请求已处理"}
			}
		} else {
			name := "delegate_task"
			args := map[string]any{"bot_id": target, "task": "生成共享报告"}
			if child {
				name = "file_write"
				args = map[string]any{"path": "shared/report.md", "content": "共享报告内容"}
			} else {
				if !strings.Contains(system, `"bot_id":"`+target+`"`) {
					t.Error("缺少公开同事目录")
					http.Error(w, "missing colleague", 400)
					return
				}
				if strings.Contains(system, "Private recipient instructions.") {
					t.Error("发送方取得目标私有指令")
					http.Error(w, "private context", 400)
					return
				}
				if strings.Contains(last.Content, "异步") {
					name = "message_bot"
					args = map[string]any{"bot_id": target, "message": "生成共享报告", "reply_required": false}
				}
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Error(err)
				return
			}
			delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "action", "type": "function", "function": map[string]string{"name": name, "arguments": string(encoded)}}}}
			reason = "tool_calls"
		}
		encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 10}})
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
	session, err := server.Auth.Login(ctx, password, "私信测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	provider, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "私信模型", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000000)
	supports := true
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: provider.ID, ModelKey: "fixture", DisplayName: "协作模型", ContextWindow: 16384, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, SupportsTools: &supports, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	sender, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher", MessagingPolicy: &domain.MessagingPolicy{CanInitiate: true, AcceptDM: true, AcceptGroupInvite: true}, ToolPolicy: &domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"message_bot", "delegate_task"}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := server.Bots.Create(ctx, service.BotInput{Template: "writer", Soul: "Private recipient instructions.", ToolPolicy: &domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"file_write"}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target = peer.ID
	handler := server.Handler()
	host := httptest.NewServer(handler)
	defer host.Close()
	conversations := map[string]string{}
	for index, content := range []string{"请委派整理报告", "请再次委派整理报告", "请异步生成报告", "请再次异步生成报告"} {
		response := request(t, handler, "POST", "/api/v1/conversations/"+sender.ConversationID+"/messages", agent.SendInput{Content: content}, headers)
		if response.Code != 202 {
			t.Fatal(response.Code, response.Body.String())
		}
		var sent agent.Submission
		if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
			t.Fatal(err)
		}
		streamCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		req, err := http.NewRequestWithContext(streamCtx, "GET", host.URL+"/api/v1/events?conversation_id="+sender.ConversationID, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Authorization", headers["Authorization"])
		stream, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stream.Body)
		kind := ""
		finished := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimPrefix(line, "event: ")
			}
			if kind == "turn.finished" && strings.HasPrefix(line, "data: ") && strings.Contains(line, sent.TurnID) {
				finished = true
				break
			}
		}
		closeErr := stream.Body.Close()
		cancel()
		if !finished || closeErr != nil {
			t.Fatal("私信父回合未收尾", scanner.Err(), closeErr)
		}
		run, err := server.Runtime.Run(ctx, sent.RunID)
		if err != nil || run.Status != "succeeded" || run.CostMicro != 40 {
			t.Fatal("真实工具费用或结果错误", run, err)
		}
		snapshot, err := server.Conversations.Snapshot(ctx, sender.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range snapshot.Messages {
			if message.TurnID == nil || *message.TurnID != sent.TurnID {
				continue
			}
			for _, block := range message.Blocks {
				if block.Handoff != nil {
					found = true
					child, err := server.Conversations.Snapshot(ctx, block.Handoff.ConversationID)
					if err != nil || len(child.Conversation.Members) != 2 || child.ActiveTurnID != nil {
						t.Fatal("私信快照错误", child, err)
					}
					conversations[child.Conversation.ID] = child.Conversation.Source
				}
				if index < 2 && block.ToolResult != nil {
					if block.ToolResult.IsError || !strings.Contains(block.ToolResult.Content, `"artifacts":["shared/report.md"]`) || !strings.Contains(block.ToolResult.Content, "共享报告已完成") {
						t.Fatal("委派交付结果错误", block.ToolResult)
					}
				}
			}
		}
		if !found {
			t.Fatal("缺少交接入口")
		}
	}
	delegation, pair := 0, 0
	var privateConversationID string
	for id, source := range conversations {
		if source == "delegation" {
			delegation++
		} else if source == "bot_message" {
			pair++
			privateConversationID = id
		} else {
			t.Fatal("未知私信来源", source)
		}
	}
	if delegation != 2 || pair != 1 {
		t.Fatal("委派未独立或私信未复用", conversations)
	}
	if response := request(t, handler, "GET", "/api/v1/conversations?kind=bot_dm", nil, nil); response.Code != 401 {
		t.Fatal("私信没有鉴权")
	}
	if os.Getenv("TOM_TEST_NATIVE_DM") == "1" {
		runNativeBotScenario(t, server, sender, session.DeviceToken, "direct_message_test.dart")
		snapshot, err := server.Conversations.Snapshot(ctx, privateConversationID)
		if err != nil {
			t.Fatal(err)
		}
		inputs, replies := 0, 0
		for _, message := range snapshot.Messages {
			for _, block := range message.Blocks {
				if block.Text == "请核实共享报告的来源" && message.SenderKind == "user" {
					inputs++
				}
				if block.Text == "共享报告的来源已核实" {
					replies++
					if message.SenderBotID == nil || *message.SenderBotID != sender.ID {
						t.Fatal("介入消息交给了错误的私信成员")
					}
				}
			}
		}
		if inputs != 1 || replies != 1 || snapshot.ActiveTurnID != nil {
			t.Fatal("介入请求或回包未按预期持久化", inputs, replies)
		}
	}
}
