package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit/browserfake"
	"io.yueyang/tom/internal/tools"
	"strings"
	"testing"
	"time"
)

func enableRealMessageTools(t *testing.T, r *Runtime, bot domain.Bot) domain.Bot {
	t.Helper()
	ctx := context.Background()
	bot = enableMessages(t, r, bot)
	updated, err := r.deps.Bots.Update(ctx, bot.ID, service.BotPatch{ToolPolicy: service.Field[domain.ToolPolicy]{Set: true, Value: domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"message_bot", "message_user", "file_read"}}}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := service.NewTerminals(ctx, r.deps.DB, r.deps.Events, r.deps.Bots, r.deps.Files, r.deps.Logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := terminals.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	reviewer, err := policy.NewReviewer(policy.ReviewerDependencies{DB: r.deps.DB, Settings: service.NewSettings(r.deps.DB), Catalog: r.deps.Catalog, Secrets: r.deps.Secrets, Factory: r.deps.Factory, Logger: r.deps.Logger})
	if err != nil {
		t.Fatal(err)
	}
	browsing, err := service.NewBrowser(ctx, service.BrowserDependencies{DB: r.deps.DB, Bots: r.deps.Bots, Events: r.deps.Events, Logger: r.deps.Logger, Factory: func(_ context.Context, options browser.Options) (service.BrowserEngine, error) {
		return browserfake.New(options), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := browsing.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	r.deps.Tools, err = tools.New(r.deps.DB, r.deps.Events, r.deps.Files, service.NewApprovals(r.deps.DB, r.deps.Events), r.deps.Secrets, terminals, browsing, reviewer, r.deps.Memories)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func emitNamedTool(emit func(provider.Event) error, name, id string, input any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if err := emit(provider.Event{Kind: "tool_use_start", ToolCallID: id, ToolName: name}); err != nil {
		return err
	}
	return emit(provider.Event{Kind: "tool_use_end", ToolCallID: id, InputDelta: string(encoded)})
}

func TestRecipientExecutesOwnToolsAndDeliversBeforeSenderFinishes(t *testing.T) {
	for _, failRecipient := range []bool{false, true} {
		t.Run(map[bool]string{false: "成功交付", true: "接收者失败"}[failRecipient], func(t *testing.T) {
			target := ""
			paused := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			r, sender := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "Name: 接收猫咪\n") {
					if failRecipient {
						return provider.Usage{}, errors.New("测试目标执行失败")
					}
					if result := lastToolResult(req); result != nil {
						if result.IsError || !strings.Contains(result.Content, "只有接收者的工作空间有这份结果") {
							t.Error("未由接收猫咪读取自己的文件", result)
						}
						return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "我独立核验的结果。"})
					}
					return provider.Usage{}, emitNamedTool(emit, "file_read", "read-recipient", map[string]any{"path": "notes/check.txt"})
				}
				if result := lastToolResult(req); result != nil {
					if failRecipient && !result.IsError {
						t.Error("目标失败却报告成功")
					}
					if !failRecipient && (result.IsError || !strings.Contains(result.Content, "\"delivered_to_user\":true") || strings.Contains(result.Content, "我独立核验的结果")) {
						t.Error("发送者应只得到真实交付回执", result)
					}
					close(paused)
					select {
					case <-release:
					case <-ctx.Done():
						return provider.Usage{}, ctx.Err()
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已转交。"})
				}
				return provider.Usage{}, emitNamedTool(emit, "message_bot", "delegate-original", map[string]any{"bot_id": target, "message": "请你读取自己的 notes/check.txt 并把核验结果直接发给用户。", "deliver_to_user": true})
			}), DefaultLimits())
			sender = enableRealMessageTools(t, r, sender)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer", Name: "接收猫咪", ToolPolicy: &domain.ToolPolicy{Approval: "loose", AllowedTools: []string{"file_read", "message_user"}}}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			if _, err := r.deps.Files.Write(context.Background(), peer.ID, "notes/check.txt", "只有接收者的工作空间有这份结果", true, func(context.Context, string, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			sent := submit(t, r, sender)
			select {
			case <-paused:
			case <-time.After(5 * time.Second):
				t.Fatal("没有取得交付回执")
			}
			var author, runID, content string
			err = r.deps.DB.QueryRow("SELECT sender_bot_id,run_id,blocks_json FROM messages WHERE conversation_id=? AND sender_kind='bot'", peer.ConversationID).Scan(&author, &runID, &content)
			if failRecipient {
				if err == nil {
					t.Fatal("失败结果不应投递到用户会话")
				}
			} else {
				if err != nil || author != peer.ID || !strings.Contains(content, "我独立核验的结果") {
					t.Fatal("未以真实接收者身份投递", err, author, content)
				}
				var executedBy string
				if err := r.deps.DB.QueryRow("SELECT r.bot_id FROM tool_calls t JOIN runs r ON r.id=t.run_id WHERE t.tool_name='file_read' AND t.status='succeeded'").Scan(&executedBy); err != nil || executedBy != peer.ID {
					t.Fatal("工具归属错误", executedBy, err)
				}
			}
			parent, err := r.Run(context.Background(), sent.RunID)
			if err != nil || parent.Status != "running" {
				t.Fatal("应在发送者结束前投递", parent.Status, err)
			}
			var notices int
			if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=? AND sender_kind='system'", peer.ConversationID).Scan(&notices); err != nil || notices != 1 {
				t.Fatal("接收通知未即时持久化", notices, err)
			}
		})
	}
}

func TestMessageUserDoesNotAddressTomOrStartChildRun(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if result := lastToolResult(req); result != nil {
			if result.IsError || !strings.Contains(result.Content, "\"delivered_to_user\":true") {
				t.Error(result)
			}
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已送达。"})
		}
		return provider.Usage{}, emitNamedTool(emit, "message_user", "notify-human", map[string]any{"message": "给用户的结果"})
	}), DefaultLimits())
	bot = enableRealMessageTools(t, r, bot)
	if _, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer", Name: "Tom"}, "user"); err != nil {
		t.Fatal(err)
	}
	sent := submit(t, r, bot)
	if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
		t.Fatal(run)
	}
	var children, posted int
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL").Scan(&children); err != nil || children != 0 {
		t.Fatal(children, err)
	}
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE id IN (SELECT id FROM tool_calls WHERE tool_name='message_user') AND conversation_id=? AND sender_bot_id=?", bot.ConversationID, bot.ID).Scan(&posted); err != nil || posted != 1 {
		t.Fatal(posted, err)
	}
}

func TestUserMessageRequiresCurrentRunAndDeduplicatesDelivery(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Usage, error) {
		close(started)
		select {
		case <-release:
			return provider.Usage{}, nil
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
	}), DefaultLimits())
	sent := submit(t, r, bot)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("任务未启动")
	}
	r.mu.Lock()
	parent := r.jobs[sent.RunID]
	r.mu.Unlock()
	messenger := runMessenger{runtime: r, parent: parent}
	run, err := r.Run(context.Background(), sent.RunID)
	if err != nil {
		t.Fatal(err)
	}
	forged := run
	forged.BotID = "other"
	if _, err := messenger.SendUserMessage(context.Background(), forged, "call", "结果"); err == nil {
		t.Fatal("允许冒充其他猫咪")
	}
	if _, err := messenger.SendUserMessage(context.Background(), run, "missing", "结果"); err == nil {
		t.Fatal("没有有效工具调用仍投递")
	}
	if _, err := r.deps.DB.Exec("INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES('human-call',?,0,'message_user','{}','running',0)", run.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reply, err := messenger.SendUserMessage(context.Background(), run, "human-call", "结果")
		if err != nil || !reply.DeliveredToUser || reply.ConversationID != bot.ConversationID {
			t.Fatal(reply, err)
		}
	}
	var count int
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE id='human-call'").Scan(&count); err != nil || count != 1 {
		t.Fatal("重复投递", count, err)
	}
}
