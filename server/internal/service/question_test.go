package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
)

type questionOutcome struct {
	answer domain.QuestionAnswer
	err    error
}

func startQuestion(s *Approvals, ctx context.Context, run domain.Run, callID string, options []string) <-chan questionOutcome {
	done := make(chan questionOutcome, 1)
	go func() {
		answer, err := s.AskUser(ctx, run, callID, domain.UserQuestion{Question: "请确认输出形式", Options: options})
		done <- questionOutcome{answer, err}
	}()
	return done
}

func waitQuestions(t *testing.T, s *Approvals, count int) []ApprovalSummary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		wake := s.events.Wake()
		items, err := s.List(ctx, "pending", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == count {
			return items
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("问题未进入等待状态")
		}
	}
}

func TestQuestionChoiceFreeTextSkipTimeoutAndCancellation(t *testing.T) {
	for _, mode := range []string{"choice", "text", "deny", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, run, callID := approvalFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "timeout" {
				s.questionTimeout = 150 * time.Millisecond
			}
			done := startQuestion(s, ctx, run, callID, []string{"简报", "详报"})
			items := waitQuestions(t, s, 1)
			if mode != "timeout" && items[0].ExpiresAt.Sub(items[0].CreatedAt) < 23*time.Hour {
				t.Fatal("问题未使用独立期限")
			}
			if items[0].TurnID != run.TurnID || items[0].WaitingQuestions != 1 {
				t.Fatal("缺少回合关联", items[0])
			}
			switch mode {
			case "choice", "text":
				note := "简报"
				if mode == "text" {
					note = "请输出待讨论清单"
				}
				if err := s.Decide(ctx, items[0].ID, ApprovalDecision{Decision: "approve", Note: note, Remember: &RememberDecision{Scope: "global"}}, "user"); err == nil {
					t.Fatal("问题允许记住规则")
				}
				if err := s.Decide(ctx, items[0].ID, ApprovalDecision{Decision: "approve"}, "user"); err == nil {
					t.Fatal("问题允许空回答")
				}
				if err := s.Decide(ctx, items[0].ID, ApprovalDecision{Decision: "approve", Note: note}, "user"); err != nil {
					t.Fatal(err)
				}
			case "deny":
				if err := s.Decide(ctx, items[0].ID, ApprovalDecision{Decision: "deny"}, "user"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			outcome := <-done
			if mode == "choice" || mode == "text" {
				if outcome.err != nil || outcome.answer.Text == "" || (mode == "choice" && (outcome.answer.Choice == nil || *outcome.answer.Choice != "简报")) || (mode == "text" && outcome.answer.Choice != nil) {
					t.Fatal(outcome)
				}
			} else if mode == "cancel" {
				if !errors.Is(outcome.err, context.Canceled) {
					t.Fatal(outcome.err)
				}
			} else {
				groupError(t, outcome.err, "USER_NO_RESPONSE")
			}
			var runStatus, turnStatus string
			if err := s.db.QueryRow("SELECT r.status,t.status FROM runs r JOIN turns t ON t.id=r.turn_id WHERE r.id=?", run.ID).Scan(&runStatus, &turnStatus); err != nil || runStatus != "running" || turnStatus != "running" {
				t.Fatal("问题收尾未恢复", runStatus, turnStatus, err)
			}
			if err := s.Decide(context.Background(), items[0].ID, ApprovalDecision{Decision: "approve", Note: "迟到回答"}, "user"); err == nil {
				t.Fatal("允许重复或迟到回答")
			}
		})
	}
}

func TestQuestionConcurrentAnswersAcceptOnlyOneActualAnswer(t *testing.T) {
	s, run, callID := approvalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := startQuestion(s, ctx, run, callID, nil)
	id := waitQuestions(t, s, 1)[0].ID
	type decisionResult struct {
		note string
		err  error
	}
	results := make(chan decisionResult, 8)
	for index := range 8 {
		go func() {
			note := fmt.Sprintf("实际回答%d", index)
			results <- decisionResult{note, s.Decide(ctx, id, ApprovalDecision{Decision: "approve", Note: note}, "user")}
		}()
	}
	winner, accepted := "", 0
	for range 8 {
		result := <-results
		if result.err == nil {
			winner, accepted = result.note, accepted+1
		} else {
			groupError(t, result.err, "APPROVAL_RESOLVED")
		}
	}
	outcome := <-done
	if accepted != 1 || outcome.err != nil || outcome.answer.Text != winner || outcome.answer.Choice != nil {
		t.Fatal("并发回答被覆盖或重复接受", accepted, outcome)
	}
}

func TestQuestionFIFOAtomicBulkSnapshotAndAuditRollback(t *testing.T) {
	s, run, callID := approvalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	first := startQuestion(s, ctx, run, callID, []string{"简报", "详报"})
	firstID := waitQuestions(t, s, 1)[0].ID
	secondRun := run
	secondRun.ID = ids.New()
	secondCall := ids.New()
	if _, err := s.db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,'group','running','fixture','Answer a question.',0)`, secondRun.ID, run.TurnID, run.ConversationID, run.BotID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES(?,?,0,'ask_user','{}','pending',0)`, secondCall, secondRun.ID); err != nil {
		t.Fatal(err)
	}
	second := startQuestion(s, ctx, secondRun, secondCall, []string{"表格", "文字"})
	items := waitQuestions(t, s, 2)
	if items[0].ID != firstID || items[0].WaitingQuestions != 2 {
		t.Fatal("问题没有按先进先出展示", items)
	}
	err := s.Decide(ctx, items[1].ID, ApprovalDecision{Decision: "approve", Note: "简报"}, "user")
	groupError(t, err, "USER_QUESTION_QUEUED")
	input := ApprovalDecision{Decision: "approve", Note: "简报", ApplyToTurn: true, QuestionIDs: []string{firstID}}
	err = s.Decide(ctx, firstID, input, "user")
	groupError(t, err, "USER_QUESTIONS_CHANGED")
	input.QuestionIDs = []string{firstID, items[1].ID}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_question_answer BEFORE INSERT ON audit_log WHEN NEW.action='approval.decide' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, firstID, input, "user"); err == nil {
		t.Fatal("回答审计失败未回滚")
	}
	var answered int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM approvals WHERE answer_json IS NOT NULL OR status!='pending'").Scan(&answered); err != nil || answered != 0 {
		t.Fatal("批量回答遗留部分状态", answered, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_question_answer"); err != nil {
		t.Fatal(err)
	}
	if err = s.Decide(ctx, firstID, input, "user"); err != nil {
		t.Fatal(err)
	}
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.answer.Choice == nil || b.answer.Choice != nil || a.answer.Text != "简报" || b.answer.Text != "简报" {
		t.Fatal(a, b)
	}
	var audit string
	if err = s.db.QueryRow("SELECT GROUP_CONCAT(detail_json) FROM audit_log WHERE action='approval.decide'").Scan(&audit); err != nil || strings.Contains(audit, "简报") {
		t.Fatal("回答正文进入审计", err)
	}
	page, err := s.List(ctx, "approved", "", 1)
	if err != nil || len(page) != 1 {
		t.Fatal(err)
	}
	next, err := s.List(ctx, "approved", page[0].ID, 1)
	if err != nil || len(next) != 1 || next[0].ID == page[0].ID {
		t.Fatal("历史分页错误", next, err)
	}
}

func TestQuestionValidationAndUnrelatedBulkDecisions(t *testing.T) {
	for _, input := range []domain.UserQuestion{{Question: " "}, {Question: strings.Repeat("x", 4001)}, {Question: "格式", Options: []string{"重复", " 重复 "}}, {Question: "格式", Options: make([]string, 9)}, {Question: "格式", Options: []string{""}}, {Question: "格式", Options: []string{strings.Repeat("x", 201)}}} {
		if _, err := NormalizeQuestion(input); err == nil {
			t.Fatal("无效提问被接受")
		}
	}
	s, run, call := approvalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startQuestion(s, ctx, run, call, nil)
	items := waitQuestions(t, s, 1)
	for _, input := range []ApprovalDecision{{Decision: "approve", Note: "答复", QuestionIDs: []string{items[0].ID}}, {Decision: "approve", Note: "答复", ApplyToTurn: true, QuestionIDs: []string{items[0].ID, items[0].ID}}, {Decision: "approve", Note: strings.Repeat("文", 334)}, {Decision: "deny", Note: "不会保存的说明"}} {
		if err := s.Decide(ctx, items[0].ID, input, "user"); err == nil {
			t.Fatal("无效回答被接受")
		}
	}
	cancel()
	if result := <-done; !errors.Is(result.err, context.Canceled) {
		t.Fatal(result.err)
	}
}
