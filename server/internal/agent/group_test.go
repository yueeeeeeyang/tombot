package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/orchestrator"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

func TestGroupContextHasEqualMembersAndAdministrator(t *testing.T) {
	j := &job{brief: "Current request", group: &orchestrator.Task{Group: domain.Group{ID: "group", Members: []domain.GroupMember{{BotID: "a", Name: "小黑"}, {BotID: "b", Name: "小黄"}}}}}
	text, err := groupSystem(j)
	if err != nil || !strings.Contains(text, "All cats are equal members") || !strings.Contains(text, "user is the group administrator") || !strings.Contains(text, "Current request") || strings.Contains(text, `"role"`) {
		t.Fatal(text, err)
	}
}

func createTestGroup(t *testing.T, r *Runtime, first domain.Bot, budget int64) domain.Group {
	t.Helper()
	members := []string{first.ID}
	for _, template := range []string{"writer", "assistant"} {
		bot, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: template}, "user")
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, bot.ID)
	}
	group, err := service.NewGroups(r.deps.DB, r.deps.Events).Create(context.Background(), service.GroupInput{Name: "协作测试", Purpose: "研究与核验", Members: members, MaxCostMicro: &budget}, "user")
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func waitGroupTurn(t *testing.T, r *Runtime, id string) (status, code string, cost int64) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		wake := r.deps.Events.Wake()
		err := r.deps.DB.QueryRow("SELECT status,COALESCE(error_code,''),cost_micro FROM turns WHERE id=?", id).Scan(&status, &code, &cost)
		if err != nil {
			t.Fatal(err)
		}
		if status != "queued" && status != "running" && status != "waiting_approval" {
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			t.Fatal("群回合未结束")
		}
	}
}

func TestGroupParallelHistoryCompletionOrderAndCost(t *testing.T) {
	started := make(chan int, 3)
	release := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		index := int(calls.Add(1)) - 1
		encoded, err := json.Marshal(req.Messages)
		if err != nil {
			return provider.Usage{}, err
		}
		if !strings.Contains(string(encoded), "公开历史") || strings.Contains(string(encoded), "私有内容") || strings.Contains(string(encoded), "member-result") {
			return provider.Usage{}, fmt.Errorf("群上下文未隔离：%s", encoded)
		}
		started <- index
		select {
		case <-release[index]:
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
		return provider.Usage{InputTokens: 10, OutputTokens: 5}, emit(provider.Event{Kind: "text_delta", Text: fmt.Sprintf("member-result-%d %s", index, strings.Repeat("已核验的结论。", 8))})
	}), DefaultLimits())
	group := createTestGroup(t, r, bot, domain.DefaultGroupBudgetMicro)
	if err := store.Transact(context.Background(), r.deps.DB, func(tx *sql.Tx) error {
		message := domain.Message{ID: ids.New(), ConversationID: group.ConversationID, SenderKind: "bot", SenderBotID: &bot.ID, Status: "complete", CreatedAt: time.Now(), Blocks: []domain.MessageBlock{{Kind: "text", Text: "公开历史"}, {Kind: "thinking", Text: "私有内容"}, {Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: "prior", Content: "私有内容"}}}}
		return service.AppendMessage(context.Background(), tx, &message)
	}); err != nil {
		t.Fatal(err)
	}
	sent, err := r.Submit(context.Background(), group.ConversationID, SendInput{Content: "并行核验"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("成员未并行启动")
		}
	}
	_, err = r.Submit(context.Background(), group.ConversationID, SendInput{Content: "重复请求"}, "user")
	requireCode(t, err, "TURN_IN_PROGRESS")
	for _, index := range []int{2, 0, 1} {
		close(release[index])
		deadline := time.NewTimer(5 * time.Second)
		for {
			wake := r.deps.Events.Wake()
			var count int
			if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE conversation_id=? AND status='complete' AND blocks_json LIKE ?", group.ConversationID, fmt.Sprintf("%%member-result-%d%%", index)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == 1 {
				break
			}
			select {
			case <-wake:
			case <-deadline.C:
				t.Fatal("成员结果未完成")
			}
		}
		deadline.Stop()
	}
	status, code, cost := waitGroupTurn(t, r, sent.TurnID)
	if status != "succeeded" || code != "" || cost != 45 {
		t.Fatalf("群收尾异常：%s %s %d", status, code, cost)
	}
	messages, err := service.NewConversations(r.deps.DB, r.deps.Events).Messages(context.Background(), group.ConversationID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, message := range messages {
		if message.RunID != nil && message.SenderKind == "bot" {
			results = append(results, message.Blocks[0].Text)
		}
	}
	if len(results) != 3 || !strings.HasPrefix(results[0], "member-result-2") || !strings.HasPrefix(results[1], "member-result-0") || !strings.HasPrefix(results[2], "member-result-1") {
		t.Fatal("发言未按完成顺序排列", results)
	}
	var count int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE turn_id=? AND source='group' AND plan_role='member' AND status='succeeded'", sent.TurnID).Scan(&count); err != nil || count != 3 {
		t.Fatal("成员 Run 归属错误", count, err)
	}
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM memory_index").Scan(&count); err != nil || count != 0 {
		t.Fatal("自动写入了私有记忆", count, err)
	}
}

// 成员失败仍保留其他成员原话与用量，不创建隐藏的协调者模型请求。
func TestGroupEqualMembersPreserveFailuresAndUsage(t *testing.T) {
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		if strings.HasPrefix(req.System, "You are the router") || strings.HasPrefix(req.System, "You are the group coordinator") {
			return provider.Usage{}, errors.New("出现协调者")
		}

		return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: "已完成"})
	}), DefaultLimits())
	group := createTestGroup(t, r, bot, domain.DefaultGroupBudgetMicro)
	// 明确停用一位成员的模型，保证失败分支不依赖模板显示名。
	if _, err := r.deps.DB.Exec("UPDATE bots SET model_id=NULL WHERE id=?", group.Members[1].BotID); err != nil {
		t.Fatal(err)
	}
	sent, err := r.Submit(context.Background(), group.ConversationID, SendInput{Content: "逐一回答"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	status, code, cost := waitGroupTurn(t, r, sent.TurnID)
	if status != "failed" || code != "GROUP_MEMBER_FAILED" || calls.Load() != 2 || cost != 4 {
		t.Fatal(status, code, cost, calls.Load())
	}
	var completed int
	if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE turn_id=? AND sender_kind='bot' AND status='complete'", sent.TurnID).Scan(&completed); err != nil || completed != 2 {
		t.Fatal("其他成员结果丢失", completed, err)
	}
}

func TestGroupBudgetIdleMentionsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"budget", "idle", "mention", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{}, 3)
			limits := DefaultLimits()
			limits.Concurrent = 1
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				calls.Add(1)
				started <- struct{}{}
				if scenario == "cancel" {
					<-ctx.Done()
					return provider.Usage{InputTokens: 3}, ctx.Err()
				}
				return provider.Usage{InputTokens: 3}, emit(provider.Event{Kind: "text_delta", Text: "已确认"})
			}), limits)
			budget := domain.DefaultGroupBudgetMicro
			if scenario == "budget" {
				budget = 2
			}
			group := createTestGroup(t, r, bot, budget)
			input := SendInput{Content: "执行任务"}
			if scenario == "mention" {
				input.Mentions = []string{bot.ID}
			}
			sent, err := r.Submit(context.Background(), group.ConversationID, input, "user")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancel" {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("成员未开始")
				}
				if err = r.Cancel(context.Background(), sent.TurnID, "user"); err != nil {
					t.Fatal(err)
				}
			}
			status, code, cost := waitGroupTurn(t, r, sent.TurnID)
			switch scenario {
			case "budget":
				if status != "failed" || code != "TURN_BUDGET_EXCEEDED" || calls.Load() != 1 || cost != 3 {
					t.Fatal(status, code, cost, calls.Load())
				}
			case "idle":
				if status != "succeeded" || calls.Load() != 3 {
					t.Fatal(status, calls.Load())
				}
			case "mention":
				if status != "succeeded" || calls.Load() != 1 {
					t.Fatal(status, calls.Load())
				}
				_, err = r.Submit(context.Background(), group.ConversationID, SendInput{Content: "错误提及", Mentions: []string{bot.ID, bot.ID}}, "user")
				requireCode(t, err, "INVALID_GROUP_MENTION")
			case "cancel":
				if status != "canceled" || cost != 3 {
					t.Fatal(status, code, cost)
				}
				var active int
				if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE turn_id=? AND status IN('queued','running','waiting_approval')", sent.TurnID).Scan(&active); err != nil || active != 0 {
					t.Fatal("回合取消后仍有活动 Run", active, err)
				}
			}
		})
	}
}

func TestGroupCancelMentionAndRecoverTurnWithoutRun(t *testing.T) {
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return provider.Usage{InputTokens: 3}, ctx.Err()
	}), DefaultLimits())
	group := createTestGroup(t, r, bot, domain.DefaultGroupBudgetMicro)
	sent, err := r.Submit(context.Background(), group.ConversationID, SendInput{Content: "取消提及", Mentions: []string{bot.ID}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("成员未开始")
	}
	if err = r.Cancel(context.Background(), sent.TurnID, "user"); err != nil {
		t.Fatal(err)
	}
	status, _, cost := waitGroupTurn(t, r, sent.TurnID)
	if status != "canceled" || cost != 3 || calls.Load() != 1 {
		t.Fatal("提及取消未正确收尾", status, cost, calls.Load())
	}
	r.Close()
	if _, err = r.deps.DB.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('unstarted-group-turn',?,2,'user','queued',0)`, group.ConversationID); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(context.Background(), r.deps, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	status, code, cost := waitGroupTurn(t, recovered, "unstarted-group-turn")
	if status != "interrupted" || code != "RUN_INTERRUPTED" || cost != 0 || calls.Load() != 1 {
		t.Fatal("无 Run 的回合未中断或被重放", status, code, cost, calls.Load())
	}
}
