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

func approveRoutine(t *testing.T, routines *Routines, _ *Approvals, input RoutineInput) domain.Routine {
	t.Helper()
	routine, err := routines.Commit(context.Background(), input, "device:test")
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
	routines, _, _, bot := routineFixture(t)
	input := routineInput(bot)
	routine, err := routines.Commit(ctx, input, "device:test")
	if err != nil {
		t.Fatal(err)
	}
	if routine.TargetMode != "new" || routine.OverlapPolicy != "skip" || routine.CatchUp != "once" || routine.Timezone != "Asia/Shanghai" || !routine.IsEnabled || routine.NextRunAt == nil {
		t.Fatal("默认值错误", routine)
	}
	updated, err := routines.UpdateNow(ctx, routine.ID, RoutinePatch{Revision: routine.Revision, Name: Field[string]{Set: true, Value: "新名称"}}, "device:test")
	if err != nil || updated.Revision != routine.Revision+1 || updated.Name != "新名称" {
		t.Fatal(updated, err)
	}
	_, err = routines.UpdateNow(ctx, routine.ID, RoutinePatch{Revision: routine.Revision, Name: Field[string]{Set: true, Value: "过期名称"}}, "device:test")
	groupError(t, err, "ROUTINE_CHANGED")
	if err = routines.Delete(ctx, updated.ID, updated.Revision, "device:test"); err != nil {
		t.Fatal(err)
	}
	_, err = routines.Get(ctx, updated.ID)
	groupError(t, err, "NOT_FOUND")
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
	input.Revision = 0
	if _, err = routines.Commit(ctx, input, "device:test"); err == nil {
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
