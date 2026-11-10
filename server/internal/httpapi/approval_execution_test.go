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

	"io.yueyang/tom/internal/agent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestHTTPToolApprovalFeedsResultBackToModel(t *testing.T) {
	for _, toolName := range []string{"file_write", "computer_shell", "terminal_session", "browser"} {
		decisions := []string{"approve"}
		if toolName == "computer_shell" {
			decisions = append(decisions, "cancel_running")
		}
		for _, decision := range decisions {
			t.Run(toolName+"/"+decision, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var input struct {
						Messages []struct {
							Role    string          `json:"role"`
							Content json.RawMessage `json:"content"`
						} `json:"messages"`
						Tools []json.RawMessage `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						http.Error(w, "invalid", 400)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if calls.Add(1) == 1 {
						if len(input.Tools) != 2 || !strings.Contains(string(input.Tools[0]), toolName) || !strings.Contains(string(input.Tools[1]), "report_progress") {
							t.Error("业务工具和进度工具声明未传入模型")
						}
						argumentsMap := map[string]any{"path": "notes/report.md", "content": "已审批的报告"}
						if toolName == "computer_shell" {
							argumentsMap = map[string]any{"command": "printf '已审批的报告' > notes/report.md"}
						}
						if decision == "cancel_running" {
							argumentsMap = map[string]any{"command": `sleep 60 & printf '%s' "$!" > notes/child.tmp; mv notes/child.tmp notes/child.pid; wait`}
						}
						if toolName == "terminal_session" {
							argumentsMap = map[string]any{"action": "open", "title": "协作终端"}
						}
						if toolName == "browser" {
							argumentsMap = map[string]any{"action": "open", "url": "https://example.com"}
						}
						arguments, err := json.Marshal(argumentsMap)
						if err != nil {
							t.Error(err)
							return
						}
						chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "upstream-call", "type": "function", "function": map[string]any{"name": toolName, "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}}})
						if err != nil {
							t.Error(err)
							return
						}
						if _, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk); err != nil {
							t.Error(err)
						}
						return
					}
					if decision == "cancel_running" {
						t.Error("取消后再次调用模型")
						return
					}
					last := input.Messages[len(input.Messages)-1]
					marker := map[string]string{"file_write": "sha256", "computer_shell": "exit_code", "terminal_session": "tom-bot-", "browser": "page_revision"}[toolName]
					if last.Role != "tool" || !strings.Contains(string(last.Content), marker) {
						t.Error("工具结果未正确回灌", last.Role)
					}
					if _, err := fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已处理工具结果。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"); err != nil {
						t.Error(err)
					}
				}))
				defer upstream.Close()
				server, code := fixture(t, true)
				password := testkit.Random(t)
				if err := server.Auth.Setup(password, code); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				session, err := server.Auth.Login(ctx, password, "审批测试", "macos", nil)
				if err != nil {
					t.Fatal(err)
				}
				headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
				p, err := server.Catalog.CreateProvider(ctx, service.ProviderInput{Kind: "openai_compat", DisplayName: "本地测试", BaseURL: upstream.URL + "/v1"}, "user")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = server.Catalog.CreateModel(ctx, service.ModelInput{ProviderID: p.ID, ModelKey: "fixture", DisplayName: "工具模型", ContextWindow: 8192, MaxOutput: 512, IsDefault: true}, "user"); err != nil {
					t.Fatal(err)
				}
				bot, err := server.Bots.Create(ctx, service.BotInput{Template: "writer", ToolPolicy: &domain.ToolPolicy{Approval: "standard", AllowedTools: []string{toolName}}}, "user")
				if err != nil {
					t.Fatal(err)
				}
				handler := server.Handler()
				prompt := "把报告写入 notes/report.md"
				if toolName == "browser" {
					prompt = "打开示例网站供我查看"
				}
				before, err := server.Conversations.Snapshot(ctx, bot.ConversationID)
				if err != nil {
					t.Fatal(err)
				}
				response := request(t, handler, "POST", "/api/v1/conversations/"+bot.ConversationID+"/messages", agent.SendInput{Content: prompt}, headers)
				if response.Code != 202 {
					t.Fatal("发送失败", response.Code)
				}
				var sent agent.Submission
				if err = json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
					t.Fatal(err)
				}
				if decision == "cancel_running" {
					cancelExecutingShell(t, server, bot, sent, headers)
					if calls.Load() != 1 {
						t.Fatal("取消后再次调用模型")
					}
				} else {
					for {
						wake := server.Bus.Wake()
						run, err := server.Runtime.Run(ctx, sent.RunID)
						if err != nil {
							t.Fatal(err)
						}
						if run.Status == "succeeded" {
							break
						}
						if run.Status == "failed" {
							t.Fatal("执行失败", run.ErrorCode)
						}
						select {
						case <-wake:
						case <-ctx.Done():
							t.Fatal("工具执行未完成")
						}
					}
					if calls.Load() != 2 {
						t.Fatal("模型循环调用次数错误")
					}
				}
				response = request(t, handler, "GET", "/api/v1/runs/"+sent.RunID, nil, headers)
				var detail struct {
					ToolCalls []agent.ToolCall `json:"tool_calls"`
				}
				if response.Code != 200 {
					t.Fatal("工具调用详情无法读取", response.Code)
				}
				if err = json.Unmarshal(response.Body.Bytes(), &detail); err != nil || len(detail.ToolCalls) != 1 {
					t.Fatal("调用状态不完整", detail.ToolCalls, err)
				}
				pending := &domain.ToolActivity{ID: detail.ToolCalls[0].ID, RunID: sent.RunID, Step: detail.ToolCalls[0].Step, Name: detail.ToolCalls[0].Name}
				checkToolTimeline(t, server, bot.ConversationID, before.EventCursor, pending, decision)
				if decision == "cancel_running" {
					return
				}
				file, err := server.Files.Read(ctx, bot.ID, "bot", "notes/report.md")
				if (toolName == "file_write" || toolName == "computer_shell") && (err != nil || !strings.Contains(file.Content, "已审批的报告")) {
					t.Fatal("工具未写入文件", err)
				}
				if toolName == "terminal_session" {
					terminals, err := server.Terminals.List(ctx, bot.ID, "", 50)
					if err != nil || len(terminals) != 1 || terminals[0].ExpiresAt == nil {
						t.Fatal("终端创建或回合后保留时间错误", err)
					}
				}
				if toolName == "browser" {
					browserTabs, err := server.Browser.List(ctx, bot.ID)
					if err != nil || len(browserTabs) != 1 || browserTabs[0].Controller != "bot" || browserTabs[0].Status != "active" {
						t.Fatal("浏览器创建结果或回合后保留错误", err)
					}
				}
				if detail.ToolCalls[0].Status != "succeeded" {
					t.Fatal("调用状态不完整", detail.ToolCalls)
				}
			})
		}
	}
}
