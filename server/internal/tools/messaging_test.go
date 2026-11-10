package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
)

type messengerFunc func(context.Context, domain.Run, string, domain.BotMessageRequest) (domain.BotMessageReply, error)

func (f messengerFunc) SendBotMessage(ctx context.Context, run domain.Run, id string, input domain.BotMessageRequest) (domain.BotMessageReply, error) {
	return f(ctx, run, id, input)
}

func messagingFixture(t *testing.T, approval string) (*Executor, domain.Bot, domain.Run, domain.Bot) {
	t.Helper()
	root := t.TempDir()
	e, bot, run := fixtureAt(t, approval, root)
	bot.ToolPolicy.AllowedTools = append(bot.ToolPolicy.AllowedTools, "message_bot", "delegate_task")
	bot.MessagingPolicy.CanInitiate = true
	encoded, err := json.Marshal(bot.MessagingPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.Exec("UPDATE bots SET messaging_policy_json=? WHERE id=?", string(encoded), bot.ID); err != nil {
		t.Fatal(err)
	}
	peer, err := service.NewBots(e.db, e.events, root).Create(context.Background(), service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	return e, bot, run, peer
}

func TestMessagingApprovalBindsRecipientAndSharedContext(t *testing.T) {
	for _, scenario := range []string{"approve", "deny", "changed_file", "changed_target"} {
		t.Run(scenario, func(t *testing.T) {
			e, bot, run, peer := messagingFixture(t, "standard")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			writer := bot
			writer.ToolPolicy.Approval = "loose"
			if result := execute(t, e, writer, run, "file_write", map[string]any{"path": "shared/brief.md", "content": "明确共享的上下文"}); result.IsError {
				t.Fatal(result)
			}
			var called atomic.Int32
			messenger := messengerFunc(func(ctx context.Context, current domain.Run, id string, input domain.BotMessageRequest) (domain.BotMessageReply, error) {
				called.Add(1)
				if current.ID != run.ID || input.BotID != peer.ID || !input.ReplyRequired || len(input.ContextFiles) != 1 || input.ContextFiles[0].Content != "明确共享的上下文" || input.TargetBinding == "" {
					t.Error("投递上下文不正确", input)
				}
				return domain.BotMessageReply{Delivered: true, Summary: "处理完成", Artifacts: []string{"shared/brief.md"}}, nil
			})
			results := make(chan domain.ToolResultBlock, 1)
			failures := make(chan error, 1)
			go func() {
				result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Messenger: messenger}, domain.ToolUseBlock{ID: ids.New(), Name: "message_bot", Input: map[string]any{"bot_id": peer.ID, "message": "核验共享说明", "context_paths": []string{"shared/brief.md"}}})
				results <- result
				failures <- err
			}()
			approval := pending(t, e)
			if called.Load() != 0 {
				t.Fatal("审批前发生投递")
			}
			decision := "approve"
			expected := ""
			switch scenario {
			case "deny":
				decision = "deny"
				expected = "TOOL_DENIED"
			case "changed_file":
				expected = "TOOL_TARGET_CHANGED"
				if result := execute(t, e, writer, run, "file_write", map[string]any{"path": "shared/brief.md", "content": "审批后改变的内容"}); result.IsError {
					t.Fatal(result)
				}
			case "changed_target":
				expected = "TOOL_TARGET_CHANGED"
				if _, err := e.db.Exec("UPDATE bots SET soul='Changed role instructions.' WHERE id=?", peer.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.approvals.Decide(ctx, approval.ID, service.ApprovalDecision{Decision: decision}, "user"); err != nil {
				t.Fatal(err)
			}
			result := <-results
			if err := <-failures; err != nil {
				t.Fatal(err)
			}
			if expected == "" {
				if result.IsError || called.Load() != 1 {
					t.Fatal(result, called.Load())
				}
			} else if !result.IsError || !strings.Contains(result.Content, expected) || called.Load() != 0 {
				t.Fatal("失效审批仍被投递", result, called.Load())
			}
		})
	}
}

func TestMessagingRejectsPrivatePathsPoliciesAndMissingArtifacts(t *testing.T) {
	e, bot, run, peer := messagingFixture(t, "loose")
	ctx := context.Background()
	calls := 0
	messenger := messengerFunc(func(context.Context, domain.Run, string, domain.BotMessageRequest) (domain.BotMessageReply, error) {
		calls++
		return domain.BotMessageReply{Delivered: true, Artifacts: []string{"shared/missing.md"}}, nil
	})
	invoke := func(input map[string]any) domain.ToolResultBlock {
		t.Helper()
		result, err := e.Execute(ctx, domain.ToolInvocation{Run: run, Bot: bot, Messenger: messenger}, domain.ToolUseBlock{ID: ids.New(), Name: "message_bot", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, path := range []string{"notes/private.md", "shared/../private.md", "shared//brief.md", "/shared/brief.md"} {
		result := invoke(map[string]any{"bot_id": peer.ID, "message": "核验", "context_paths": []string{path}})
		if !result.IsError || calls != 0 {
			t.Fatal("私有或非规范路径通过", path, result)
		}
	}
	for _, policy := range []struct{ id, key, code string }{{bot.ID, "can_initiate", "BOT_MESSAGE_NOT_ALLOWED"}, {peer.ID, "accept_dm", "BOT_DM_DENIED"}} {
		if _, err := e.db.Exec("UPDATE bots SET messaging_policy_json=json_set(messaging_policy_json,?,json('false')) WHERE id=?", "$."+policy.key, policy.id); err != nil {
			t.Fatal(err)
		}
		result := invoke(map[string]any{"bot_id": peer.ID, "message": "核验"})
		if !result.IsError || !strings.Contains(result.Content, policy.code) || calls != 0 {
			t.Fatal(result, calls)
		}
		if _, err := e.db.Exec("UPDATE bots SET messaging_policy_json=json_set(messaging_policy_json,?,json('true')) WHERE id=?", "$."+policy.key, policy.id); err != nil {
			t.Fatal(err)
		}
	}
	result := invoke(map[string]any{"bot_id": peer.ID, "message": "核验"})
	if !result.IsError || calls != 1 {
		t.Fatal("不存在的产物被当作交付成功", result, calls)
	}
}
