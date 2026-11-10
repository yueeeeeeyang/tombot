package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func createAgentRoutine(t *testing.T, r *Runtime, input service.RoutineInput) (*service.Routines, domain.Routine) {
	t.Helper()
	ctx := context.Background()
	routines := service.NewRoutines(r.deps.DB, r.deps.Events, r)
	proposal, err := routines.Propose(ctx, "", input, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.NewApprovals(r.deps.DB, r.deps.Events).Decide(ctx, proposal.ApprovalID, service.ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	routine, err := routines.Get(ctx, proposal.RoutineID)
	if err != nil {
		t.Fatal(err)
	}
	return routines, routine
}

func TestRoutineBotNewConversationsAtomicDispatchAndPrimaryIdentity(t *testing.T) {
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: "例程结果"})
	}), DefaultLimits())
	routines, routine := createAgentRoutine(t, r, service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "例程资料", Cron: "0 8 * * *", Prompt: "整理资料", Target: domain.RoutineTarget{BotID: bot.ID}}})
	ctx := context.Background()
	first, err := routines.RunNow(ctx, routine.ID, routine.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	status, _, cost := waitGroupTurn(t, r, first.TurnID)
	if status != "succeeded" || cost != 2 {
		t.Fatal(status, cost)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.SubmitRoutine(ctx, first.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	second, err := routines.RunNow(ctx, routine.ID, routine.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ = waitGroupTurn(t, r, second.TurnID)
	if status != "succeeded" || calls.Load() != 2 || first.ConversationID == second.ConversationID || first.ConversationID == bot.ConversationID {
		t.Fatal("独立会话或派发去重失败", first, second, calls.Load())
	}
	current, err := r.deps.Bots.Get(ctx, bot.ID)
	if err != nil || current.ConversationID != bot.ConversationID {
		t.Fatal("例程取代主会话", current, err)
	}
	var count int
	if err = r.deps.DB.QueryRow(`SELECT COUNT(*) FROM runs WHERE source='routine' AND status='succeeded'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("运行来源错误", count, err)
	}
	if err = r.deps.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id=?`, bot.ConversationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("例程内容泄漏到主会话", err, count)
	}
}

func TestRoutineGroupCompletionAndToolsUseActualConversation(t *testing.T) {
	started := make(chan int, 3)
	release := make(chan struct{})
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		index := int(calls.Add(1))
		started <- index
		if index > 1 {
			select {
			case <-ctx.Done():
				return provider.Usage{}, ctx.Err()
			case <-release:
			}
		}
		return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "text_delta", Text: "群组例程进展"})
	}), DefaultLimits())
	group := createTestGroup(t, r, bot, "all", false, 1000)
	routines, routine := createAgentRoutine(t, r, service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "群组晨会", Cron: "0 8 * * *", Prompt: "协同整理材料", Target: domain.RoutineTarget{GroupID: group.ID}}})
	ctx := context.Background()
	trigger, err := routines.RunNow(ctx, routine.ID, routine.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("群成员未开始")
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		wake := r.deps.Events.Wake()
		var completed int
		if err = r.deps.DB.QueryRow(`SELECT COUNT(*) FROM runs WHERE turn_id=? AND status='succeeded'`, trigger.TurnID).Scan(&completed); err != nil {
			t.Fatal(err)
		}
		if completed == 1 {
			break
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatal("首个成员未完成")
		}
	}
	var status string
	if err = r.deps.DB.QueryRow(`SELECT status FROM turns WHERE id=?`, trigger.TurnID).Scan(&status); err != nil || status != "running" {
		t.Fatal("首个成员提前结束例程群回合", status, err)
	}
	groups := service.NewGroups(r.deps.DB, r.deps.Events)
	_, err = groups.Update(ctx, group.ID, service.GroupPatch{Revision: group.Revision}, "user")
	requireCode(t, err, "TURN_IN_PROGRESS")
	var run domain.Run
	if err = r.deps.DB.QueryRow(`SELECT id,bot_id,model_id FROM runs WHERE turn_id=? AND status='running' LIMIT 1`, trigger.TurnID).Scan(&run.ID, &run.BotID, &run.ModelID); err != nil {
		t.Fatal(err)
	}
	run.TurnID, run.ConversationID, run.Source = trigger.TurnID, trigger.ConversationID, "routine"
	if _, err = r.deps.DB.Exec(`INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES('routine-post',?,1,'post_to_group','{}','running',0)`, run.ID); err != nil {
		t.Fatal(err)
	}
	posted, err := groups.PostMessage(ctx, group.ID, "routine-post", run, "当前例程中的公开进展")
	if err != nil || posted.ConversationID != trigger.ConversationID {
		t.Fatal("群工具未使用实际例程会话", posted, err)
	}
	close(release)
	status, _, cost := waitGroupTurn(t, r, trigger.TurnID)
	if status != "succeeded" || cost != 6 {
		t.Fatal(status, cost)
	}
	var count int
	if err = r.deps.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id=?`, group.ConversationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("群例程写入主会话", count, err)
	}
	if err = groups.Delete(ctx, group.ID, group.Revision, "user"); err != nil {
		t.Fatal("删除未清理所有例程会话", err)
	}
}

func TestRoutinePinnedQueueWaitsForPreviousTurn(t *testing.T) {
	started := make(chan struct{}, 2)
	r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Usage, error) {
		started <- struct{}{}
		<-ctx.Done()
		return provider.Usage{}, ctx.Err()
	}), DefaultLimits())
	routines, routine := createAgentRoutine(t, r, service.RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "排队任务", Cron: "0 8 * * *", Prompt: "持续处理", Target: domain.RoutineTarget{BotID: bot.ID}, TargetMode: "pinned", OverlapPolicy: "queue"}})
	ctx := context.Background()
	first, err := routines.RunNow(ctx, routine.ID, routine.Revision, "user")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("首个任务未执行")
	}
	requireCode(t, routines.Delete(ctx, routine.ID, routine.Revision, "user"), "TURN_IN_PROGRESS")
	second, err := routines.RunNow(ctx, routine.ID, routine.Revision, "user")
	if err != nil || second.Status != "pending" {
		t.Fatal(second, err)
	}
	if err = r.Cancel(ctx, first.TurnID, "user"); err != nil {
		t.Fatal(err)
	}
	waitGroupTurn(t, r, first.TurnID)
	if err = routines.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("后续任务未从队列入队")
	}
	runs, err := routines.Runs(ctx, routine.ID, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == second.ID {
			if run.TurnID == "" || run.ConversationID != first.ConversationID {
				t.Fatal(run)
			}
			if err = r.Cancel(ctx, run.TurnID, "user"); err != nil {
				t.Fatal(err)
			}
			waitGroupTurn(t, r, run.TurnID)
		}
	}
}
