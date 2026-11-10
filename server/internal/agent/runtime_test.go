package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/secret"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/testkit"
	ledger "io.yueyang/tom/internal/usage"
	"io.yueyang/tom/internal/workspace"
)

type script func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error)

func (s script) Stream(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
	return s(ctx, req, emit)
}

func runtimeFixture(t *testing.T, scripted provider.Provider, limits Limits) (*Runtime, domain.Bot) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	catalog := service.NewCatalog(db)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "shared"), 0700); err != nil {
		t.Fatal(err)
	}
	bots := service.NewBots(db, events, root)
	files, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	memories := service.NewMemories(db, bots, files, events)
	p, err := catalog.CreateProvider(context.Background(), service.ProviderInput{Kind: "openai_compat", DisplayName: "测试", BaseURL: "http://127.0.0.1:1234/v1"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	price := int64(1_000_000)
	_, err = catalog.CreateModel(context.Background(), service.ModelInput{ProviderID: p.ID, ModelKey: "fixture", DisplayName: "测试模型", ContextWindow: 8192, MaxOutput: 1024, InputPriceMicro: &price, OutputPriceMicro: &price, IsDefault: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := bots.Create(context.Background(), service.BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(context.Background(), Dependencies{Files: files, DB: db, Events: events, Catalog: catalog, Bots: bots, Memories: memories, Secrets: secret.New(db, []byte(testkit.Random(t))[:32]), Factory: func(domain.Provider, []byte) provider.Provider { return scripted }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, bot
}
func submit(t *testing.T, r *Runtime, bot domain.Bot) Submission {
	t.Helper()
	sent, err := r.Submit(context.Background(), bot.ConversationID, SendInput{Content: "请整理材料"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	return sent
}
func waitRun(t *testing.T, r *Runtime, id string) domain.Run {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		wake := r.deps.Events.Wake()
		run, err := r.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "queued" && run.Status != "running" && run.Status != "waiting_approval" {
			return run
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatal("执行未结束")
		}
	}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *domain.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("错误码不匹配: %v，需要 %s", err, code)
	}
}

func TestRuntimeStreamPersistenceAndUsage(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if len(req.Messages) != 1 || !strings.Contains(req.System, "Identity and role") {
			return provider.Usage{}, errors.New("上下文缺失")
		}
		for _, text := range []string{"已核对", "全部资料。"} {
			if err := emit(provider.Event{Kind: "text_delta", Text: text}); err != nil {
				return provider.Usage{}, err
			}
		}
		return provider.Usage{InputTokens: 10, OutputTokens: 8}, nil
	}), DefaultLimits())
	sent := submit(t, r, bot)
	run := waitRun(t, r, sent.RunID)
	if run.Status != "succeeded" || run.CostMicro != 18 {
		t.Fatalf("执行或用量失败: %+v", run)
	}
	messages, err := service.NewConversations(r.deps.DB, r.deps.Events).Messages(context.Background(), bot.ConversationID, 0, 50)
	if err != nil || len(messages) != 2 || messages[1].Blocks[0].Text != "已核对全部资料。" || messages[1].Status != "complete" {
		t.Fatalf("流式消息错误: %+v %v", messages, err)
	}
	var turnCost, count int
	if err = r.deps.DB.QueryRow("SELECT cost_micro FROM turns WHERE id=?", sent.TurnID).Scan(&turnCost); err != nil || turnCost != 18 {
		t.Fatal("回合费用未累计", err)
	}
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM usage_events WHERE run_id=?", sent.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatal("调用用量不唯一", err)
	}
	events, _, err := r.deps.Events.Read(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "text.delta" {
			found = true
		}
	}
	if !found {
		t.Fatal("流式增量未持久化")
	}
}

func TestRuntimeConflictQueueCancellationAndDisconnect(t *testing.T) {
	started := make(chan struct{}, 4)
	var active, peak atomic.Int32
	r, first := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Usage, error) {
		current := active.Add(1)
		defer active.Add(-1)
		if current > peak.Load() {
			peak.Store(current)
		}
		started <- struct{}{}
		<-ctx.Done()
		return provider.Usage{}, ctx.Err()
	}), func() Limits { l := DefaultLimits(); l.Concurrent = 1; return l }())
	request, cancel := context.WithCancel(context.Background())
	a, err := r.Submit(request, first.ConversationID, SendInput{Content: "继续工作"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-started
	if active.Load() != 1 {
		t.Fatal("HTTP 上下文取消错误地停止了执行")
	}
	_, err = r.Submit(context.Background(), first.ConversationID, SendInput{Content: "重复提交"}, "user")
	requireCode(t, err, "TURN_IN_PROGRESS")
	second, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	b := submit(t, r, second)
	run, err := r.Run(context.Background(), b.RunID)
	if err != nil || run.Status != "queued" {
		t.Fatal("未遵守全局并发", err)
	}
	snapshot, err := service.NewConversations(r.deps.DB, r.deps.Events).Snapshot(t.Context(), second.ConversationID)
	if err != nil || snapshot.ActiveTurnStatus == nil || *snapshot.ActiveTurnStatus != "queued" || len(snapshot.ActiveRuns) != 1 || snapshot.ActiveRuns[0].ID != b.RunID {
		t.Fatal("排队回合未进入快照", err)
	}
	var owner, turn string
	if err = r.deps.DB.QueryRow("SELECT json_extract(payload_json,'$.bot_id'),json_extract(payload_json,'$.turn_id') FROM events WHERE kind='run.status' AND conversation_id=? AND json_extract(payload_json,'$.status')='queued'", second.ConversationID).Scan(&owner, &turn); err != nil || owner != second.ID || turn != b.TurnID {
		t.Fatal("排队事件不能定位对应 Bot 和回合", err)
	}
	if err = r.Cancel(context.Background(), b.TurnID, "user"); err != nil {
		t.Fatal(err)
	}
	if waitRun(t, r, b.RunID).Status != "canceled" {
		t.Fatal("排队任务未取消")
	}
	if err = r.Cancel(context.Background(), a.TurnID, "user"); err != nil {
		t.Fatal(err)
	}
	if waitRun(t, r, a.RunID).Status != "canceled" || peak.Load() != 1 {
		t.Fatal("上游未取消或并发超限")
	}
	var count int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE sender_kind='user' AND conversation_id=?", first.ConversationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("409 留下了重复用户消息", err)
	}
}

func TestRuntimeConcurrentSubmissions(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) (provider.Usage, error) {
		<-ctx.Done()
		return provider.Usage{}, ctx.Err()
	}), DefaultLimits())
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			_, err := r.Submit(context.Background(), bot.ConversationID, SendInput{Content: "并发请求"}, "user")
			if err == nil {
				successes.Add(1)
			} else {
				requireCode(t, err, "TURN_IN_PROGRESS")
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("同会话创建了多个活跃回合")
	}
}

func TestRuntimeToolsDeniedAndStepLimits(t *testing.T) {
	for _, maxSteps := range []int{1, 3} {
		t.Run(string(rune('0'+maxSteps)), func(t *testing.T) {
			var calls atomic.Int32
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				call := calls.Add(1)
				if call == 2 {
					last := req.Messages[len(req.Messages)-1].Blocks[0].ToolResult
					if last == nil || !last.IsError || !strings.Contains(last.Content, "TOOL_DENIED") {
						return provider.Usage{}, errors.New("拒绝结果没有回灌")
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "当前无法使用该工具。"})
				}
				if err := emit(provider.Event{Kind: "tool_use_start", ToolCallID: "call-1", ToolName: "unavailable"}); err != nil {
					return provider.Usage{}, err
				}
				return provider.Usage{}, emit(provider.Event{Kind: "tool_use_end", ToolCallID: "call-1", InputDelta: `{"path":"notes/a.md"}`})
			}), func() Limits { l := DefaultLimits(); l.MaxSteps = maxSteps; return l }())
			run := waitRun(t, r, submit(t, r, bot).RunID)
			if maxSteps == 1 {
				if run.ErrorCode == nil || *run.ErrorCode != "RUN_MAX_STEPS" {
					t.Fatalf("步骤上限未生效: %+v", run)
				}
			} else if run.Status != "succeeded" {
				t.Fatalf("拒绝后循环未完成: %+v", run)
			}
			var count int
			if err := r.deps.DB.QueryRow(`SELECT COUNT(*) FROM messages m,json_each(m.blocks_json) b WHERE m.run_id=? AND json_extract(b.value,'$.kind')='tool_result'`, run.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("工具结果不唯一", err)
			}
		})
	}
}

func TestRuntimeTimerFlushAndPartialFailure(t *testing.T) {
	gate := make(chan struct{})
	sentText := make(chan struct{})
	r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if err := emit(provider.Event{Kind: "text_delta", Text: "已收到的部分"}); err != nil {
			return provider.Usage{}, err
		}
		if err := emit(provider.Event{Kind: "text_delta", Text: "等待定时落库的增量"}); err != nil {
			return provider.Usage{}, err
		}
		close(sentText)
		select {
		case <-gate:
			return provider.Usage{OutputTokens: 4}, domain.NewError(502, "PROVIDER_STREAM_INTERRUPTED", "The upstream connection ended.")
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
	}), DefaultLimits())
	sent := submit(t, r, bot)
	<-sentText
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		wake := r.deps.Events.Wake()
		var text string
		if err := r.deps.DB.QueryRow("SELECT blocks_json FROM messages WHERE run_id=? ORDER BY seq DESC LIMIT 1", sent.RunID).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "已收到的部分等待定时落库的增量") {
			break
		}
		select {
		case <-wake:
		case <-deadline.C:
			t.Fatal("上游暂停时未按 500ms 刷新")
		}
	}
	close(gate)
	run := waitRun(t, r, sent.RunID)
	if run.Status != "failed" || run.CostMicro != 4 {
		t.Fatalf("中断被误报成功: %+v", run)
	}
	var status string
	if err := r.deps.DB.QueryRow("SELECT status FROM messages WHERE run_id=? AND sender_kind='bot'", sent.RunID).Scan(&status); err != nil || status != "interrupted" {
		t.Fatal("部分响应没有标记中断", err)
	}
}

func TestRuntimeTimeoutAndBudget(t *testing.T) {
	for _, mode := range []string{"timeout", "budget"} {
		t.Run(mode, func(t *testing.T) {
			l := DefaultLimits()
			l.BudgetMicro = 1
			if mode == "timeout" {
				l.WallTime = 20 * time.Millisecond
			}
			r, bot := runtimeFixture(t, script(func(ctx context.Context, _ provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if mode == "timeout" {
					<-ctx.Done()
					return provider.Usage{}, ctx.Err()
				}
				if err := emit(provider.Event{Kind: "tool_use_start", ToolCallID: "budget-call", ToolName: "unavailable"}); err != nil {
					return provider.Usage{}, err
				}
				return provider.Usage{InputTokens: 2}, emit(provider.Event{Kind: "tool_use_end", ToolCallID: "budget-call", InputDelta: `{}`})
			}), l)
			run := waitRun(t, r, submit(t, r, bot).RunID)
			expected := "RUN_BUDGET_EXCEEDED"
			if mode == "timeout" {
				expected = "RUN_TIMEOUT"
			}
			if run.ErrorCode == nil || *run.ErrorCode != expected {
				t.Fatalf("上限错误: %+v", run)
			}
		})
	}
}

func TestRuntimeRejectsInvalidAndMissingModel(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	}), DefaultLimits())
	for _, content := range []string{"", strings.Repeat("x", 65537)} {
		_, err := r.Submit(context.Background(), bot.ConversationID, SendInput{Content: content}, "user")
		requireCode(t, err, "INVALID_INPUT")
	}
	_, err := r.deps.Bots.Update(context.Background(), bot.ID, service.BotPatch{ModelID: service.Field[*string]{Set: true}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Submit(context.Background(), bot.ConversationID, SendInput{Content: "你好"}, "user")
	requireCode(t, err, "MODEL_NOT_CONFIGURED")
}

func TestCostRoundingCachedAndOverflow(t *testing.T) {
	cache := int64(100_000)
	cost, estimated, err := ledger.Cost(domain.Model{InputPriceMicro: 500_000, OutputPriceMicro: 1_000_000, CacheReadPriceMicro: &cache}, provider.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 5})
	if err != nil || estimated || cost != 3 {
		t.Fatalf("整数费用错误 %d %v %v", cost, estimated, err)
	}
	cost, estimated, err = ledger.Cost(domain.Model{InputPriceMicro: 500_000}, provider.Usage{CacheReadTokens: 1})
	if err != nil || !estimated || cost != 1 {
		t.Fatal("缓存回退费用错误")
	}
	_, _, err = ledger.Cost(domain.Model{InputPriceMicro: math.MaxInt64}, provider.Usage{InputTokens: math.MaxInt})
	requireCode(t, err, "PROVIDER_INVALID_USAGE")
	_, _, err = ledger.Cost(domain.Model{}, provider.Usage{InputTokens: -1})
	requireCode(t, err, "PROVIDER_INVALID_USAGE")
}

func TestRecoveryInterruptsDurableActiveState(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	}), DefaultLimits())
	sent := submit(t, r, bot)
	waitRun(t, r, sent.RunID)
	r.Close()
	if _, err := r.deps.DB.Exec("UPDATE runs SET status='waiting_approval' WHERE id=?", sent.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.deps.DB.Exec("UPDATE turns SET status='running' WHERE id=?", sent.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.deps.DB.Exec(`INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES('recovered-call',?,0,'file_read','{}','running',0)`, sent.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.deps.DB.Exec(`INSERT INTO approvals(id,run_id,tool_call_id,bot_id,action_class,title,detail_json,risk_level,status,expires_at,created_at) VALUES('recovered-approval',?,'recovered-call',?,'file.read','读取文件','{}','low','pending',0,0)`, sent.RunID, bot.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), r.deps, r.limits)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	run, err := restarted.Run(context.Background(), sent.RunID)
	if err != nil || run.Status != "interrupted" {
		t.Fatal("启动未中断旧执行", err)
	}
	var status string
	if err = r.deps.DB.QueryRow("SELECT status FROM tool_calls WHERE id='recovered-call'").Scan(&status); err != nil || status != "canceled" {
		t.Fatal("工具未取消", err)
	}
	if err = r.deps.DB.QueryRow("SELECT status FROM approvals WHERE id='recovered-approval'").Scan(&status); err != nil || status != "expired" {
		t.Fatal("审批未过期", err)
	}
	messages, err := service.NewConversations(r.deps.DB, r.deps.Events).Messages(context.Background(), bot.ConversationID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(messages)
	if err != nil || !strings.Contains(string(encoded), "已中断") {
		t.Fatal("缺少恢复说明", err)
	}
}

func TestContextCompactionAndSummaryAccounting(t *testing.T) {
	for _, failSummary := range []bool{false, true} {
		t.Run(fmt.Sprint(failSummary), func(t *testing.T) {
			var summaries atomic.Int32
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.HasPrefix(req.System, "Summarize the historical") {
					summaries.Add(1)
					if failSummary {
						return provider.Usage{}, domain.NewError(502, "PROVIDER_UNAVAILABLE", "The model is unavailable.")
					}
					return provider.Usage{InputTokens: 2, OutputTokens: 3}, emit(provider.Event{Kind: "text_delta", Text: "Earlier material was reviewed; the current task remains open."})
				}
				if req.Messages[len(req.Messages)-1].Blocks[0].Text != "请整理材料" {
					return provider.Usage{}, errors.New("当前任务被裁剪")
				}
				return provider.Usage{InputTokens: 1, OutputTokens: 1}, emit(provider.Event{Kind: "text_delta", Text: "已整理。"})
			}), DefaultLimits())
			for seq := 1; seq <= 6; seq++ {
				turnID := ids.New()
				err := store.Transact(context.Background(), r.deps.DB, func(tx *sql.Tx) error {
					if _, err := tx.Exec("INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,?,'user','succeeded',0)", turnID, bot.ConversationID, seq); err != nil {
						return err
					}
					m := domain.Message{ID: ids.New(), ConversationID: bot.ConversationID, TurnID: &turnID, SenderKind: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: strings.Repeat("材料", 1500)}}, Status: "complete", CreatedAt: time.Now()}
					return service.AppendMessage(context.Background(), tx, &m)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			run := waitRun(t, r, submit(t, r, bot).RunID)
			if run.Status != "succeeded" || summaries.Load() != 1 {
				t.Fatalf("摘要阻塞或未执行 %+v", run)
			}
			var summary string
			var upto int64
			if err := r.deps.DB.QueryRow("SELECT summary,summary_upto_seq FROM conversations WHERE id=?", bot.ConversationID).Scan(&summary, &upto); err != nil {
				t.Fatal(err)
			}
			if !failSummary {
				if upto < 1 || summary == "" || run.CostMicro != 7 {
					t.Fatal("摘要或结算不完整", upto, run.CostMicro)
				}
			} else {
				if upto != 0 || summary != "" {
					t.Fatal("失败摘要覆盖了原状态")
				}
				var notices int
				if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM events WHERE kind='system.notice' AND conversation_id=?", bot.ConversationID).Scan(&notices); err != nil || notices != 1 {
					t.Fatal("摘要失败没有明确告警", err)
				}
			}
		})
	}
}

func TestContextRejectsOversizedCurrentTurn(t *testing.T) {
	var calls atomic.Int32
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		calls.Add(1)
		return provider.Usage{}, nil
	}), DefaultLimits())
	sent, err := r.Submit(context.Background(), bot.ConversationID, SendInput{Content: strings.Repeat("x", 60000)}, "user")
	if err != nil {
		t.Fatal(err)
	}
	run := waitRun(t, r, sent.RunID)
	if calls.Load() != 0 || run.ErrorCode == nil || *run.ErrorCode != "CONTEXT_LIMIT" {
		t.Fatalf("超量上下文仍发往供应商 %+v", run)
	}
}

func TestUsageAccumulationCannotOverflowSQLiteInteger(t *testing.T) {
	r, bot := runtimeFixture(t, script(func(context.Context, provider.Request, func(provider.Event) error) (provider.Usage, error) {
		return provider.Usage{}, nil
	}), DefaultLimits())
	run := waitRun(t, r, submit(t, r, bot).RunID)
	if _, err := r.deps.DB.Exec("UPDATE runs SET cost_micro=? WHERE id=?", int64(math.MaxInt64-1), run.ID); err != nil {
		t.Fatal(err)
	}
	model, err := r.deps.Catalog.Model(context.Background(), run.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = ledger.Record(context.Background(), r.deps.DB, run, model, provider.Usage{InputTokens: 2})
	requireCode(t, err, "PROVIDER_INVALID_USAGE")
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&after); err != nil || before != after {
		t.Fatal("费用事务未完整回滚", err)
	}
	var stored int64
	if err = r.deps.DB.QueryRow("SELECT cost_micro FROM runs WHERE id=?", run.ID).Scan(&stored); err != nil || stored != math.MaxInt64-1 {
		t.Fatal("整数账本发生溢出", err)
	}
}
