package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestRoutineHTTPApprovalManualBotGroupAndHistory(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) < 2 {
			t.Error("例程模型请求无效", err)
			http.Error(w, "invalid request", 400)
			return
		}
		if !strings.Contains(input.Messages[len(input.Messages)-1].Content, "整理") {
			t.Error("例程指令未进入模型上下文")
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"例程已完成\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"); err != nil {
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
	session, err := server.Auth.Login(ctx, password, "例程测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	provider, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "例程测试", BaseURL: upstream.URL + "/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1000000)
	if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: provider.ID, ModelKey: "fixture", DisplayName: "例程模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, IsDefault: true}, "user"); err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	summary := false
	group, err := server.Groups.Create(ctx, service.GroupInput{Name: "例程小组", Routing: "all", Members: []string{bot.ID}, AutoSummary: &summary}, "user")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	if response := request(t, handler, "GET", "/api/v1/routines", nil, nil); response.Code != 401 {
		t.Fatal("例程缺少鉴权")
	}
	for _, target := range []domain.RoutineTarget{{BotID: bot.ID}, {GroupID: group.ID}} {
		input := service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "例程整理", Cron: "0 8 * * *", Prompt: "整理资料", Target: target}}
		response := request(t, handler, "POST", "/api/v1/routines", input, headers)
		if response.Code != 202 {
			t.Fatal(response.Code, response.Body.String())
		}
		var proposal service.RoutineProposal
		if err = json.Unmarshal(response.Body.Bytes(), &proposal); err != nil {
			t.Fatal(err)
		}
		response = request(t, handler, "GET", "/api/v1/routines/"+proposal.RoutineID, nil, headers)
		if response.Code != 404 {
			t.Fatal("未审批例程提前生效", response.Code)
		}
		response = request(t, handler, "POST", "/api/v1/approvals/"+proposal.ApprovalID+"/decision", service.ApprovalDecision{Decision: "approve"}, headers)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		routine, err := server.Routines.Get(ctx, proposal.RoutineID)
		if err != nil {
			t.Fatal(err)
		}
		response = request(t, handler, "POST", "/api/v1/routines/"+routine.ID+"/run-now", routineRevision{Revision: routine.Revision}, headers)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		var trigger domain.RoutineRun
		if err = json.Unmarshal(response.Body.Bytes(), &trigger); err != nil {
			t.Fatal(err)
		}
		if trigger.Status != "triggered" || trigger.TurnID == "" || trigger.ConversationID == bot.ConversationID || trigger.ConversationID == group.ConversationID {
			t.Fatal(trigger)
		}
		deadline := time.After(8 * time.Second)
		for {
			wake := server.Bus.Wake()
			turns, err := server.Runtime.Turns(ctx, trigger.ConversationID, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(turns) == 1 && turns[0].Status == "succeeded" {
				if turns[0].CostMicro != 5 || turns[0].Source != "routine" {
					t.Fatal(turns)
				}
				break
			}
			select {
			case <-wake:
			case <-deadline:
				t.Fatal("例程 HTTP 执行未完成", turns)
			}
		}
		response = request(t, handler, "GET", "/api/v1/routines/"+routine.ID+"/runs", nil, headers)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"turn_status":"succeeded"`) {
			t.Fatal(response.Code, response.Body.String())
		}
		input.Revision = routine.Revision
		input.Name = "修改后例程"
		response = request(t, handler, "PATCH", "/api/v1/routines/"+routine.ID, map[string]any{"revision": routine.Revision, "name": "修改后例程"}, headers)
		if response.Code != 202 {
			t.Fatal(response.Code, response.Body.String())
		}
		if err = json.Unmarshal(response.Body.Bytes(), &proposal); err != nil {
			t.Fatal(err)
		}
		response = request(t, handler, "POST", "/api/v1/approvals/"+proposal.ApprovalID+"/decision", service.ApprovalDecision{Decision: "deny"}, headers)
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		response = request(t, handler, "DELETE", fmt.Sprintf("/api/v1/routines/%s?revision=%d", routine.ID, routine.Revision), nil, headers)
		if response.Code != 204 {
			t.Fatal(response.Code, response.Body.String())
		}
		if _, err = server.Conversations.Get(ctx, trigger.ConversationID); err != nil {
			t.Fatal("删除例程误删已完成转录", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("例程重复执行", calls.Load())
	}
}
