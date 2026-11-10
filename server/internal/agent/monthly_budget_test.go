package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func setMonthlyLimit(t *testing.T, r *Runtime) {
	t.Helper()
	var patch service.SettingsPatch
	if err := json.Unmarshal([]byte(`{"monthly_budget":{"revision":0,"limit_micro":1}}`), &patch); err != nil {
		t.Fatal(err)
	}
	if _, err := service.NewSettings(r.deps.DB).Update(context.Background(), patch, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestMonthlyAdmissionDirectGroupAndRoutine(t *testing.T) {
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: "已完成"})
	}), DefaultLimits())
	setMonthlyLimit(t, r)
	group := createTestGroup(t, r, bot, "all", false, 100)
	routines, routine := createAgentRoutine(t, r, service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "预算验证", Cron: "0 8 * * *", Prompt: "整理材料", Target: domain.RoutineTarget{BotID: bot.ID}}})
	first := submit(t, r, bot)
	status, _, cost := waitGroupTurn(t, r, first.TurnID)
	if status != "succeeded" || cost != 2 {
		t.Fatal("已接受任务未正常结算", status, cost)
	}
	for _, conversation := range []string{bot.ConversationID, group.ConversationID} {
		_, err := r.Submit(context.Background(), conversation, SendInput{Content: "新增任务"}, "user")
		var public *domain.Error
		if !errors.As(err, &public) || public.Code != "MONTHLY_BUDGET_EXCEEDED" {
			t.Fatal("超限仍接受新任务", err)
		}
	}
	trigger, err := routines.RunNow(context.Background(), routine.ID, routine.Revision, "user")
	if err != nil || trigger.Status != "failed" || trigger.ErrorCode != "MONTHLY_BUDGET_EXCEEDED" || trigger.TurnID != "" {
		t.Fatal("例程未记录预算失败", trigger, err)
	}
	var turns int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM turns").Scan(&turns); err != nil || turns != 1 || calls.Load() != 1 {
		t.Fatal("拒绝时产生回合或调用", turns, calls.Load(), err)
	}
}

func TestMonthlyAdmissionStopsChildTurnsWithoutStoppingParent(t *testing.T) {
	for _, delegation := range []bool{false, true} {
		t.Run(map[bool]string{false: "私信", true: "委派"}[delegation], func(t *testing.T) {
			target := ""
			var calls atomic.Int32
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				calls.Add(1)
				if result := lastToolResult(req); result != nil {
					if !result.IsError || !strings.Contains(result.Content, "MONTHLY_BUDGET_EXCEEDED") {
						return provider.Usage{}, errors.New("月度预算错误未回灌")
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "本月预算已满，子任务未创建"})
				}
				if delegation {
					encoded, err := json.Marshal(domain.BotMessageRequest{BotID: target, Message: "请核验", ReplyRequired: true})
					if err != nil {
						return provider.Usage{}, err
					}
					if err = emit(provider.Event{Kind: "tool_use_start", ToolCallID: "delegate", ToolName: "delegate_task"}); err != nil {
						return provider.Usage{}, err
					}
					return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "tool_use_end", ToolCallID: "delegate", InputDelta: string(encoded)})
				}
				return provider.Usage{InputTokens: 2}, emitMessageCall(emit, target, true)
			}), DefaultLimits())
			bot = enableMessages(t, r, bot)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			setMonthlyLimit(t, r)
			first := submit(t, r, bot)
			status, code, _ := waitGroupTurn(t, r, first.TurnID)
			if status != "succeeded" || code != "" || calls.Load() != 2 {
				t.Fatal(status, code, calls.Load())
			}
			var children int
			if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL").Scan(&children); err != nil || children != 0 {
				t.Fatal("子任务绕过月度预算", children, err)
			}
		})
	}
}
