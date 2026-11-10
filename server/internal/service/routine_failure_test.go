package service

import (
	"context"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
)

func TestRoutineClaimFailureRollsBackClockAndConversation(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	routine := approveRoutine(t, routines, approvals, routineInput(bot))
	now := routine.NextRunAt.Add(time.Minute)
	if _, err := routines.db.Exec(`CREATE TRIGGER fail_routine_claim BEFORE INSERT ON routine_runs BEGIN SELECT RAISE(ABORT,'test_claim'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := routines.claim(ctx, routine.ID, "schedule", 0, *routine.NextRunAt, now, "system"); err == nil {
		t.Fatal("触发落库失败被忽略")
	}
	current, err := routines.Get(ctx, routine.ID)
	if err != nil || !current.NextRunAt.Equal(*routine.NextRunAt) {
		t.Fatal("失败抢占推进了时钟", current, err)
	}
	var count int
	if err = routines.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE source='routine'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("失败抢占遗留会话", count, err)
	}
}

func TestRoutineDisableExpiresQueueAndKeepsManualRun(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	input := routineInput(bot)
	routine := approveRoutine(t, routines, approvals, input)
	claimID, err := routines.claim(ctx, routine.ID, "manual", routine.Revision, time.Time{}, time.Now(), "user")
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	input.IsEnabled = &enabled
	input.Revision = routine.Revision
	proposal, err := routines.Propose(ctx, routine.ID, input, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	current, err := routines.Get(ctx, routine.ID)
	if err != nil || current.IsEnabled || current.NextRunAt != nil {
		t.Fatal(current, err)
	}
	claim, err := routines.run(ctx, claimID)
	if err != nil || claim.Status != "failed" || claim.ErrorCode != "ROUTINE_CHANGED" {
		t.Fatal("配置变化后旧队列仍可执行", claim, err)
	}
	manual, err := routines.RunNow(ctx, current.ID, current.Revision, "user")
	if err != nil || manual.Status != "triggered" {
		t.Fatal("停用定时错误地禁止手动触发", manual, err)
	}
}

func TestRoutineInvalidApprovalExpiryAndDeletedTarget(t *testing.T) {
	ctx := context.Background()
	routines, approvals, bots, bot := routineFixture(t)
	for _, input := range []RoutineInput{
		{RoutineDefinition: domain.RoutineDefinition{Name: "空目标", Cron: "* * * * *", Prompt: "整理"}},
		{RoutineDefinition: domain.RoutineDefinition{Name: "重复目标", Cron: "* * * * *", Prompt: "整理", Target: domain.RoutineTarget{BotID: bot.ID, GroupID: "group"}}},
		{RoutineDefinition: domain.RoutineDefinition{Name: "无调度", Cron: "0 0 30 2 *", Prompt: "整理", Target: domain.RoutineTarget{BotID: bot.ID}}},
		{RoutineDefinition: domain.RoutineDefinition{Name: "不支持策略", Cron: "* * * * *", Prompt: "整理", Target: domain.RoutineTarget{BotID: bot.ID}, OverlapPolicy: "parallel"}},
	} {
		if _, err := routines.Propose(ctx, "", input, "user"); err == nil {
			t.Fatal("接受无效例程", input.Name)
		}
	}
	proposal, err := routines.Propose(ctx, "", routineInput(bot), "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = routines.db.Exec(`UPDATE approvals SET expires_at=0 WHERE id=?`, proposal.ApprovalID); err != nil {
		t.Fatal(err)
	}
	groupError(t, approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "user"), "APPROVAL_EXPIRED")
	proposal, err = routines.Propose(ctx, "", routineInput(bot), "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = bots.Delete(ctx, bot.ID, bot.Name, "user", false); err != nil {
		t.Fatal(err)
	}
	groupError(t, approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "user"), "APPROVAL_RESOLVED")
}

func TestRoutineRecoveryDoesNotDiscardNewProcessQueue(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	routine := approveRoutine(t, routines, approvals, routineInput(bot))
	if err := routines.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	claimID, err := routines.claim(ctx, routine.ID, "manual", routine.Revision, time.Time{}, time.Now(), "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = routines.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	claim, err := routines.run(ctx, claimID)
	if err != nil || claim.Status != "triggered" {
		t.Fatal("启动恢复重复清理了新进程队列", claim, err)
	}
}

func TestRoutinePartialUpdatePreservesDisabledScheduleAndCanResetPinnedConversation(t *testing.T) {
	ctx := context.Background()
	routines, approvals, _, bot := routineFixture(t)
	if _, err := routines.db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('custom-routine','direct','routine',?,'固定历史',0,0)`, bot.ID); err != nil {
		t.Fatal(err)
	}
	input := routineInput(bot)
	enabled := false
	input.IsEnabled = &enabled
	input.TargetMode = "pinned"
	input.PinnedConversationID = "custom-routine"
	routine := approveRoutine(t, routines, approvals, input)
	proposal, err := routines.ProposeUpdate(ctx, routine.ID, RoutinePatch{Revision: routine.Revision, Name: Field[string]{Set: true, Value: "只改名称"}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	current, err := routines.Get(ctx, routine.ID)
	if err != nil || current.IsEnabled || current.NextRunAt != nil || current.Prompt != routine.Prompt || current.Cron != routine.Cron || current.PinnedConversationID != "custom-routine" {
		t.Fatal("局部更新覆盖未提供字段", current, err)
	}
	proposal, err = routines.ProposeUpdate(ctx, current.ID, RoutinePatch{Revision: current.Revision, PinnedConversationID: Field[string]{Set: true, Value: ""}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = approvals.Decide(ctx, proposal.ApprovalID, ApprovalDecision{Decision: "approve"}, "user"); err != nil {
		t.Fatal(err)
	}
	current, err = routines.Get(ctx, current.ID)
	if err != nil || current.PinnedConversationID != bot.ConversationID {
		t.Fatal("无法重置到主会话", current, err)
	}
	if _, err = routines.ProposeUpdate(ctx, current.ID, RoutinePatch{Revision: current.Revision, IsEnabled: Field[*bool]{Set: true}}, "user"); err == nil {
		t.Fatal("接受空启用状态")
	}
}
