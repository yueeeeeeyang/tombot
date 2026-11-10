package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/testkit"
)

func approvalFixture(t *testing.T) (*Approvals, domain.Run, string) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	bot, err := NewBots(db, events, t.TempDir()).Create(context.Background(), BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), ConversationID: bot.ConversationID, BotID: bot.ID}
	if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)`, run.TurnID, run.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,'chat','running','fixture','Write a report.',0)`, run.ID, run.TurnID, run.ConversationID, run.BotID); err != nil {
		t.Fatal(err)
	}
	callID := ids.New()
	if _, err = db.Exec(`INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES(?,?,0,'file_write','{}','pending',0)`, callID, run.ID); err != nil {
		t.Fatal(err)
	}
	return NewApprovals(db, events), run, callID
}

func TestApprovalTimeoutRestoresRunAndRejectsLateDecision(t *testing.T) {
	s, run, callID := approvalFixture(t)
	s.timeout = 15 * time.Millisecond
	err := s.RequestWait(context.Background(), run, callID, policy.Action{Class: "fs.write", Pattern: "out/report", Risk: "low", Title: "修改文件"})
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != "APPROVAL_TIMEOUT" {
		t.Fatalf("超时错误 %v", err)
	}
	approvals, err := s.List(context.Background(), "expired", "", 50)
	if err != nil || len(approvals) != 1 {
		t.Fatal("审批未过期", err)
	}
	var status string
	if err = s.db.QueryRow("SELECT status FROM runs WHERE id=?", run.ID).Scan(&status); err != nil || status != "running" {
		t.Fatal("超时后无法继续", err)
	}
	err = s.Decide(context.Background(), approvals[0].ID, ApprovalDecision{Decision: "approve"}, "user")
	if !errors.As(err, &public) || public.Code != "APPROVAL_RESOLVED" {
		t.Fatal("已过期审批仍可决策", err)
	}
}

func TestApprovalDecisionChecksDeadlineAndInvalidRememberAtomically(t *testing.T) {
	s, run, callID := approvalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.RequestWait(ctx, run, callID, policy.Action{Class: "fs.write", Pattern: "out/report", Risk: "low", Title: "修改文件"})
	}()
	var approval ApprovalSummary
	for {
		wake := s.events.Wake()
		items, err := s.List(ctx, "pending", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 {
			approval = items[0]
			break
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("等待审批超时")
		}
	}
	var public *domain.Error
	err := s.Decide(ctx, approval.ID, ApprovalDecision{Decision: "approve", Remember: &RememberDecision{Scope: "invalid"}}, "user")
	if !errors.As(err, &public) || public.Code != "INVALID_INPUT" {
		t.Fatal("无效记忆范围被接受", err)
	}
	if _, err = s.db.Exec("UPDATE approvals SET expires_at=0 WHERE id=?", approval.ID); err != nil {
		t.Fatal(err)
	}
	err = s.Decide(ctx, approval.ID, ApprovalDecision{Decision: "approve", Remember: &RememberDecision{Scope: "global"}}, "user")
	if !errors.As(err, &public) || public.Code != "APPROVAL_EXPIRED" {
		t.Fatal("到期后仍获准", err)
	}
	err = <-done
	if !errors.As(err, &public) || public.Code != "APPROVAL_TIMEOUT" {
		t.Fatal("到期未通知等待者", err)
	}
	rules, err := s.Rules(ctx, "", 50)
	if err != nil || len(rules) != 0 {
		t.Fatal("失败决策仍保存规则", err)
	}
}
