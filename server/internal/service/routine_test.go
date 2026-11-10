package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store"
)

type routineDispatchFunc func(context.Context, string) error

func (f routineDispatchFunc) SubmitRoutine(ctx context.Context, id string) error { return f(ctx, id) }

func routineFixture(t *testing.T) (*Routines, *Approvals, *Bots, domain.Bot) {
	t.Helper()
	_, bots, members := groupFixture(t)
	events := bus.New(bots.db)
	dispatch := routineDispatchFunc(func(ctx context.Context, id string) error {
		return store.Transact(ctx, bots.db, func(tx *sql.Tx) error {
			trigger, err := ReadRoutineRun(ctx, tx, id)
			if err != nil {
				return err
			}
			if trigger.Status != "pending" {
				return nil
			}
			if err = CheckRoutineDispatch(ctx, tx, id, trigger.ConversationID); err != nil {
				return err
			}
			turnID := ids.New()
			if _, err = tx.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,seq,source,status,created_at) SELECT ?,?,COALESCE(MAX(seq),0)+1,'routine','succeeded',? FROM turns WHERE conversation_id=?`, turnID, trigger.ConversationID, time.Now().UnixMilli(), trigger.ConversationID); err != nil {
				return err
			}
			return AttachRoutineTurn(ctx, tx, id, turnID)
		})
	})
	return NewRoutines(bots.db, events, dispatch), NewApprovals(bots.db, events), bots, members[0]
}

func approveRoutine(t *testing.T, routines *Routines, approvals *Approvals, input RoutineInput) domain.Routine {
	t.Helper()
	ctx := context.Background()
	proposal, err := routines.Propose(ctx, "", input, "device:test")
	if err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "device:test"); err != nil {
		t.Fatal(err)
	}
	routine, err := routines.Get(ctx, proposal.RoutineID)
	if err != nil {
		t.Fatal(err)
	}
	return routine
}

func routineInput(bot domain.Bot) RoutineInput {
	return RoutineInput{RoutineDefinition: domain.RoutineDefinition{Name: "资料整理", Cron: "* * * * *", Prompt: "请整理最新资料", Target: domain.RoutineTarget{BotID: bot.ID}}}
}

func TestRoutineApprovalRevisionDenialAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	input := routineInput(bot)
	proposal, err := routines.Propose(ctx, "", input, "device:test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = routines.Get(ctx, proposal.RoutineID); err == nil {
		t.Fatal("未审批配置提前生效")
	}
	if err = approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "deny"}, "device:test"); err != nil {
		t.Fatal(err)
	}
	routine := approveRoutine(t, routines, approvals, input)
	if routine.TargetMode != "new" || routine.OverlapPolicy != "skip" || routine.CatchUp != "once" || routine.Timezone != "Asia/Shanghai" || !routine.IsEnabled || routine.NextRunAt == nil {
		t.Fatal("默认值错误", routine)
	}
	input.Revision = routine.Revision
	input.Name = "新名称"
	first, err := routines.Propose(ctx, routine.ID, input, "device:test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := routines.Propose(ctx, routine.ID, input, "device:test")
	if err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, first.ApprovalID, ApprovalDecision{Decision: "approve", Remember: &RememberDecision{Scope: "global"}}, "device:test"); err == nil {
		t.Fatal("例程允许记忆批准")
	}
	if _, err = routines.db.Exec(`CREATE TRIGGER fail_routine_apply BEFORE INSERT ON audit_log WHEN NEW.action='routine.apply' BEGIN SELECT RAISE(ABORT,'test_audit'); END`); err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, first.ApprovalID, ApprovalDecision{Decision: "approve"}, "device:test"); err == nil {
		t.Fatal("审计失败未阻断")
	}
	current, err := routines.Get(ctx, routine.ID)
	if err != nil || current.Name != routine.Name {
		t.Fatal("失败审批修改了配置", err)
	}
	if _, err = routines.db.Exec(`DROP TRIGGER fail_routine_apply`); err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, first.ApprovalID, ApprovalDecision{Decision: "approve"}, "device:test"); err != nil {
		t.Fatal(err)
	}
	groupError(t, approvals.Decide(ctx, second.ApprovalID, ApprovalDecision{Decision: "approve"}, "device:test"), "ROUTINE_CHANGED")
	current, err = routines.Get(ctx, routine.ID)
	if err != nil || current.Revision != 2 || current.Name != "新名称" {
		t.Fatal(current, err)
	}
	if err = routines.Delete(ctx, current.ID, current.Revision, "device:test"); err != nil {
		t.Fatal(err)
	}
	groupError(t, approvals.Decide(ctx, second.ApprovalID, ApprovalDecision{Decision: "approve"}, "device:test"), "APPROVAL_RESOLVED")
}

func TestRoutineConcurrentClaimCatchUpAndManualSchedule(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	routine := approveRoutine(t, routines, approvals, routineInput(bot))
	now := routine.NextRunAt.Add(4 * time.Hour)
	routines.now = func() time.Time { return now }
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := routines.claim(ctx, routine.ID, "catch_up", 0, *routine.NextRunAt, now, "system")
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := routines.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	runs, err := routines.Runs(ctx, routine.ID, "", 50)
	if err != nil || len(runs) != 1 || runs[0].Status != "triggered" || runs[0].TriggerKind != "catch_up" {
		t.Fatal(runs, err)
	}
	current, err := routines.Get(ctx, routine.ID)
	if err != nil || !current.NextRunAt.After(now) {
		t.Fatal(current, err)
	}
	manual, err := routines.RunNow(ctx, routine.ID, current.Revision, "device:test")
	if err != nil || manual.Status != "triggered" || manual.ConversationID == runs[0].ConversationID {
		t.Fatal(manual, err)
	}
	after, err := routines.Get(ctx, routine.ID)
	if err != nil || !after.NextRunAt.Equal(*current.NextRunAt) {
		t.Fatal("手动运行修改定时计划", err)
	}
	if err = routines.dispatch(ctx, runs[0].ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = routines.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE source='routine'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("重复派发创建了回合", count, err)
	}
}

func TestRoutineOverlapQueueRecoveryAndTargetIsolation(t *testing.T) {
	ctx := context.Background()
	routines, approvals, bots, bot := routineFixture(t)
	input := routineInput(bot)
	input.TargetMode = "pinned"
	routine := approveRoutine(t, routines, approvals, input)
	if routine.PinnedConversationID != bot.ConversationID {
		t.Fatal("未选择主会话")
	}
	if _, err := routines.db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('busy-turn',?,1,'user','running',0)`, bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	result, err := routines.RunNow(ctx, routine.ID, routine.Revision, "device:test")
	if err != nil || result.Status != "skipped_overlap" {
		t.Fatal(result, err)
	}
	input.OverlapPolicy = "queue"
	input.Name = "排队资料"
	queued := approveRoutine(t, routines, approvals, input)
	routines.dispatcher = routineDispatchFunc(func(context.Context, string) error { return domain.NewError(409, "TURN_IN_PROGRESS", "Active turn.") })
	result, err = routines.RunNow(ctx, queued.ID, queued.Revision, "device:test")
	if err != nil || result.Status != "pending" {
		t.Fatal(result, err)
	}
	if err = routines.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	result, err = routines.run(ctx, result.ID)
	if err != nil || result.Status != "failed" || result.ErrorCode != "ROUTINE_INTERRUPTED" {
		t.Fatal("重启重放了入队意图", result, err)
	}
	other, err := bots.Create(ctx, BotInput{Name: "其他目标", Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	input.PinnedConversationID = other.ConversationID
	if _, err = routines.Propose(ctx, "", input, "device:test"); err == nil {
		t.Fatal("接受其他目标的会话")
	}
}

func TestRoutineMissedSkipAndDispatchFailureRemainVisible(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	input := routineInput(bot)
	input.CatchUp = "skip"
	routine := approveRoutine(t, routines, approvals, input)
	now := routine.NextRunAt.Add(24 * time.Hour)
	routines.now = func() time.Time { return now }
	if err := routines.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	runs, err := routines.Runs(ctx, routine.ID, "", 50)
	if err != nil || len(runs) != 1 || runs[0].Status != "skipped_missed" {
		t.Fatal(runs, err)
	}
	routines.dispatcher = routineDispatchFunc(func(context.Context, string) error { return errors.New("测试入队失败") })
	result, err := routines.RunNow(ctx, routine.ID, routine.Revision, "device:test")
	if err != nil || result.Status != "failed" || result.ErrorCode != "ROUTINE_DISPATCH_FAILED" {
		t.Fatal(result, err)
	}
}
