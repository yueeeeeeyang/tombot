package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

type questionTestTools struct {
	messageTestTools
	approvals *service.Approvals
}

func (e questionTestTools) Execute(ctx context.Context, invocation domain.ToolInvocation, use domain.ToolUseBlock) (domain.ToolResultBlock, error) {
	if use.Name != "ask_user" {
		return e.messageTestTools.Execute(ctx, invocation, use)
	}
	result := domain.ToolResultBlock{ToolCallID: use.ID}
	if _, err := e.db.ExecContext(ctx, `INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES(?,?,?,'ask_user','{}','pending',0)`, use.ID, invocation.Run.ID, invocation.Step); err != nil {
		return result, err
	}
	answer, err := invocation.UserWaiter.WaitForUser(ctx, func(ctx context.Context) (domain.QuestionAnswer, error) {
		return e.approvals.AskUser(ctx, invocation.Run, use.ID, domain.UserQuestion{Question: "需要哪种格式？", Options: []string{"简报", "表格"}})
	})
	if err != nil {
		return result, err
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return result, err
	}
	result.Content = string(encoded)
	_, err = e.db.ExecContext(ctx, "UPDATE tool_calls SET status='succeeded' WHERE id=?", use.ID)
	return result, err
}

func questionScript(_ context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
	if result := lastToolResult(req); result != nil {
		return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: result.Content})
	}
	if err := emit(provider.Event{Kind: "tool_use_start", ToolCallID: "question", ToolName: "ask_user"}); err != nil {
		return provider.Usage{}, err
	}
	return provider.Usage{}, emit(provider.Event{Kind: "tool_use_end", ToolCallID: "question", InputDelta: `{"question":"需要哪种格式？","options":["简报","表格"]}`})
}

func enableQuestions(r *Runtime) *service.Approvals {
	approvals := service.NewApprovals(r.deps.DB, r.deps.Events)
	r.deps.Tools = questionTestTools{messageTestTools{r.deps.DB, r.deps.Bots}, approvals}
	return approvals
}

func awaitQuestions(t *testing.T, r *Runtime, approvals *service.Approvals, count int) []service.ApprovalSummary {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		wake := r.deps.Events.Wake()
		questions, err := approvals.List(context.Background(), "pending", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(questions) == count {
			return questions
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatalf("待回答问题数量错误：%d，需要 %d", len(questions), count)
		}
	}
}

func TestQuestionRuntimeReleasesSlotRetainsBotAndResumesBeyondWallTime(t *testing.T) {
	limits := DefaultLimits()
	limits.Concurrent, limits.WallTime = 1, time.Second
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.Contains(req.System, "无需提问") {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "完成"})
		}
		return questionScript(ctx, req, emit)
	}), limits)
	approvals := enableQuestions(r)
	first := submit(t, r, bot)
	questions := awaitQuestions(t, r, approvals, 1)
	_, err := r.Submit(context.Background(), bot.ConversationID, SendInput{Content: "重复任务"}, "user")
	requireCode(t, err, "TURN_IN_PROGRESS")
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer", Name: "无需提问"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if run := waitRun(t, r, submit(t, r, peer).RunID); run.Status != "succeeded" {
		t.Fatal("人工等待未让出单并发槽", run)
	}
	time.Sleep(1100 * time.Millisecond)
	if err = approvals.Decide(context.Background(), questions[0].ID, service.ApprovalDecision{Decision: "approve", Note: "简报"}, "user"); err != nil {
		t.Fatal(err)
	}
	if run := waitRun(t, r, first.RunID); run.Status != "succeeded" {
		t.Fatal("人工等待被计入执行时限", run)
	}
}

func TestQuestionRuntimeGroupBulkAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			limits := DefaultLimits()
			limits.Concurrent, limits.WallTime = 1, time.Second
			r, bot := runtimeFixture(t, script(questionScript), limits)
			approvals := enableQuestions(r)
			group := createTestGroup(t, r, bot, "all", false, 100000)
			sent, err := r.Submit(context.Background(), group.ConversationID, SendInput{Content: "整理资料"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			questions := awaitQuestions(t, r, approvals, 3)
			time.Sleep(1100 * time.Millisecond)
			if cancel {
				err = r.Cancel(context.Background(), sent.TurnID, "user")
			} else {
				ids := []string{}
				for _, question := range questions {
					ids = append(ids, question.ID)
				}
				err = approvals.Decide(context.Background(), questions[0].ID, service.ApprovalDecision{Decision: "approve", Note: "简报", ApplyToTurn: true, QuestionIDs: ids}, "user")
			}
			if err != nil {
				t.Fatal(err)
			}
			status, code, _ := waitGroupTurn(t, r, sent.TurnID)
			if (!cancel && status != "succeeded") || (cancel && status != "canceled") {
				t.Fatal(status, code)
			}
			awaitQuestions(t, r, approvals, 0)
		})
	}
}

func TestQuestionRuntimeChildWaitPausesOnlyHumanTime(t *testing.T) {
	for _, wait := range []bool{true, false} {
		t.Run(fmt.Sprint(wait), func(t *testing.T) {
			var target string
			limits := DefaultLimits()
			limits.Concurrent, limits.WallTime = 1, time.Second
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "This is a one-to-one bot conversation") {
					return questionScript(ctx, req, emit)
				}
				if lastToolResult(req) != nil {
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "完成"})
				}
				return provider.Usage{}, emitMessageCall(emit, target, wait)
			}), limits)
			bot = enableMessages(t, r, bot)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			approvals := enableQuestions(r)
			sent := submit(t, r, bot)
			questions := awaitQuestions(t, r, approvals, 1)
			time.Sleep(1100 * time.Millisecond)
			if err = approvals.Decide(context.Background(), questions[0].ID, service.ApprovalDecision{Decision: "approve", Note: "简报"}, "user"); err != nil {
				t.Fatal(err)
			}
			if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
				t.Fatal("子问题等待耗尽父时限", run)
			}
		})
	}
}

func TestActiveParentClockDoesNotPauseForAsyncHumanWait(t *testing.T) {
	clock := newExecutionClock(context.Background(), 50*time.Millisecond)
	defer clock.Close()
	parent := &job{clock: clock, humanWaits: 1}
	(&Runtime{}).syncHumanClockLocked(parent)
	select {
	case <-clock.Done():
		if !errors.Is(clock.Err(), context.DeadlineExceeded) {
			t.Fatal(clock.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("仍在工作的父任务被子问题无限延长")
	}
}

func TestQuestionRuntimeShutdownExpiresQuestionWithoutReplaying(t *testing.T) {
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		return questionScript(ctx, req, emit)
	}), DefaultLimits())
	root, stop := context.WithCancel(context.Background())
	defer stop()
	r.root = root
	approvals := enableQuestions(r)
	sent := submit(t, r, bot)
	question := awaitQuestions(t, r, approvals, 1)[0]
	stop()
	r.Close()
	run := waitRun(t, r, sent.RunID)
	if run.Status != "interrupted" {
		t.Fatal("关闭时未中断人工等待", run)
	}
	awaitQuestions(t, r, approvals, 0)
	if err := approvals.Decide(context.Background(), question.ID, service.ApprovalDecision{Decision: "approve", Note: "迟到答案"}, "user"); err == nil {
		t.Fatal("关闭后接受迟到回答")
	}
	restarted, err := New(context.Background(), r.deps, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.jobs) != 0 || calls.Load() != 1 {
		t.Fatal("重启重放了提问或模型调用")
	}
}
