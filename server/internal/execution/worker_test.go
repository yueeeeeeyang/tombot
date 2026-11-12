package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io.yueyang/tom/internal/botagent"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computercontrol"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

type callerFunc func(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error)

func (call callerFunc) Call(ctx context.Context, configuration modelregistry.Configuration, credential []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
	return call(ctx, configuration, credential, input, emit)
}

type testTool struct {
	execute      func(context.Context, Invocation) (botagent.ToolResult, error)
	usesComputer bool
}

func (tool testTool) UsesComputer() bool { return tool.usesComputer }

func (tool testTool) Schema() provider.ToolSchema {
	return provider.ToolSchema{Name: "file_read", Description: "Read a file from the current run workspace.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}
}
func (tool testTool) RequiresVision() bool { return false }
func (tool testTool) Execute(ctx context.Context, invocation Invocation) (botagent.ToolResult, error) {
	return tool.execute(ctx, invocation)
}

func emitText(emit func(provider.Event) error, text string) (modelgateway.Result, error) {
	for _, event := range []provider.Event{{Kind: "text_delta", Text: text}, {Kind: "stop", StopReason: "stop"}} {
		if err := emit(event); err != nil {
			return modelgateway.Result{}, err
		}
	}
	return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
}
func emitTool(emit func(provider.Event) error, arguments string) (modelgateway.Result, error) {
	for _, event := range []provider.Event{{Kind: "tool_use_start", Index: 0, ToolCallID: "call-test", ToolName: "file_read"}, {Kind: "tool_use_delta", Index: 0, ToolCallID: "call-test", InputDelta: arguments}, {Kind: "tool_use_end", Index: 0, ToolCallID: "call-test", ToolName: "file_read", InputDelta: arguments}, {Kind: "stop", StopReason: "tool_calls"}} {
		if err := emit(event); err != nil {
			return modelgateway.Result{}, err
		}
	}
	return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
}
func (fixture fixture) plainBot(t *testing.T) bots.Bot {
	t.Helper()
	spec := fixture.first.Spec
	spec.ToolPermissions = []string{}
	bot, err := fixture.catalog.Update(context.Background(), fixture.a, fixture.first.ID, bots.UpdateRequest{ExpectedRevision: fixture.first.Revision, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return bot
}
func (fixture fixture) toolsModel(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var providerID string
	if err := fixture.pool.QueryRow(`SELECT provider_id FROM tombot.models WHERE id=$1`, fixture.modelID).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	model, err := fixture.registry.CreateModel(ctx, fixture.actor, ids.New(), modelregistry.ModelRequest{ProviderID: providerID, ModelKey: "execution-tools", DisplayName: "工具模型", ContextLimit: 4096, OutputLimit: 1024, SupportsTools: true})
	if err != nil {
		t.Fatal(err)
	}
	version, err := fixture.registry.CreateDraft(ctx, fixture.actor, ids.New(), modelregistry.DraftRequest{PolicyID: fixture.policyID, ModelID: model.ID, OutputLimit: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.registry.Publish(ctx, fixture.actor, ids.New(), fixture.policyID, version.Number, fixture.revision(t)); err != nil {
		t.Fatal(err)
	}
}
func (fixture fixture) worker(t *testing.T, caller ModelCaller, tools ...Tool) *Worker {
	t.Helper()
	return fixture.workerWithComputers(t, caller, nil, tools...)
}
func (fixture fixture) workerWithComputers(t *testing.T, caller ModelCaller, computers *computercontrol.Service, tools ...Tool) *Worker {
	t.Helper()
	if computers == nil {
		computers = fixture.computers(t, &sessionNode{}, computerOptions())
	}
	worker, err := NewWorker(fixture.service, fixture.registry, caller, tools, computers, newWorkerAgents(t), WorkerOptions{Concurrency: 2, PollInterval: 100 * time.Millisecond, RunTimeout: 10 * time.Second, ModelTimeout: 3 * time.Second, ToolTimeout: 3 * time.Second, RecordTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second, MaxModelSteps: 4, MaxToolCalls: 4, MaxOutputBytes: 8 << 10}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

type expiringDirectory struct {
	ModelDirectory
	expire func()
}

func (directory expiringDirectory) Use(ctx context.Context, selection modelregistry.Selection, action func(context.Context, modelregistry.Configuration, []byte) error) error {
	return directory.ModelDirectory.Use(ctx, selection, func(ctx context.Context, configuration modelregistry.Configuration, credential []byte) error {
		directory.expire()
		return action(ctx, configuration, credential)
	})
}

func TestWorkerRequiresContainerAgent(t *testing.T) {
	cluster := pgtest.Start(t)
	for _, mode := range []string{"启动未确认", "记账前电脑过期"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, cluster)
			bot := fixture.plainBot(t)
			fixture.send(t, fixture.a, bot)
			lease := fixture.claim(t)
			calls := 0
			worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				calls++
				return emitText(emit, "不应执行")
			}))
			if _, err := NewWorker(fixture.service, fixture.registry, worker.caller, nil, worker.computers, nil, worker.options, worker.logger); !errors.Is(err, ErrInvalidInput) {
				t.Fatal("缺少远端 Agent 仍可启动 Worker", err)
			}
			if mode == "启动未确认" {
				worker.agents.(*workerAgents).failStart = true
			} else {
				worker.directory = expiringDirectory{ModelDirectory: worker.directory, expire: func() {
					execute(t, fixture.pool, `UPDATE tombot.computer_sessions SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID)
				}}
			}
			run, err := worker.Process(context.Background(), lease)
			if err != nil || run.Status == "succeeded" || calls != 0 || worker.agents.(*workerAgents).starts != 1 {
				t.Fatal("电脑失效后回退执行模型", run.Status, calls, err)
			}
			var operations int
			if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2`, lease.UserID, lease.RunID).Scan(&operations); err != nil || operations != 0 {
				t.Fatal("电脑无效仍记入新模型操作", operations, err)
			}
		})
	}
}

func TestPersistentWorker(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("固定模型调用结算并发布回复", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		bot := fixture.plainBot(t)
		fixture.send(t, fixture.a, bot)
		lease := fixture.claim(t)
		calls := 0
		worker := fixture.worker(t, callerFunc(func(_ context.Context, configuration modelregistry.Configuration, credential []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if fixture.sessionState(t, lease) != "active" {
				t.Error("纯文本模型调用未取得电脑执行权")
			}
			if configuration.ModelKey != "execution-fixture" || len(credential) != 0 || len(input.Tools) != 0 || len(input.Messages) != 1 || input.Messages[0].Role != "user" || input.System != bot.SystemPrompt {
				t.Error("模型调用未使用所属运行快照")
			}
			return emitText(emit, "已经完成资料核对。")
		}))
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "succeeded" || calls != 1 {
			t.Fatal("持久 Worker 未完成", run.Status, calls, err)
		}
		if worker.agents.(*workerAgents).starts != 1 || fixture.sessionState(t, lease) != "released" {
			t.Fatal("纯文本运行没有经历 Agent 启动与电脑回收")
		}
		messages, err := fixture.service.Messages(ctx, fixture.a, bot.ConversationID, 0, 10)
		if err != nil || len(messages) != 2 || messages[0].Blocks[0].Text != "已经完成资料核对。" {
			t.Fatal("回复未发布", err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 12 || held != 0 {
			t.Fatal("真实用量未结算", known, held)
		}
	})
	t.Run("后台领取两个用户并有界停止", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		first := fixture.plainBot(t)
		spec := fixture.foreign.Spec
		spec.ToolPermissions = []string{}
		foreign, err := fixture.catalog.Update(ctx, fixture.b, fixture.foreign.ID, bots.UpdateRequest{ExpectedRevision: fixture.foreign.Revision, Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		one, two := fixture.send(t, fixture.a, first), fixture.send(t, fixture.b, foreign)
		worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			return emitText(emit, "后台任务已完成。")
		}))
		running, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- worker.Serve(running) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			firstRun, err := fixture.service.GetRun(ctx, fixture.a, one.RunID)
			if err != nil {
				t.Fatal(err)
			}
			secondRun, err := fixture.service.GetRun(ctx, fixture.b, two.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if firstRun.Status == "succeeded" && secondRun.Status == "succeeded" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("后台未完成两个用户的任务", firstRun.Status, secondRun.Status)
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("后台停止状态不明确", err)
			}
		case <-time.After(time.Second):
			t.Fatal("后台未停止")
		}
	})
	t.Run("工具循环在最终提交失败后不重放", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.toolsModel(t)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		calls, tools := 0, 0
		arguments := `{ "path" : "notes.txt", "counter": 9007199254740993, "scale": 1e2 }`
		worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				return emitTool(emit, arguments)
			}
			if len(input.Messages) != 3 || input.Messages[2].Blocks[0].ToolResult == nil || input.Messages[2].Blocks[0].ToolResult.Content != "Verified file contents." {
				t.Error("工具结果未进入下一次模型上下文")
			}
			return emitText(emit, "文件已核对。")
		}), testTool{execute: func(_ context.Context, invocation Invocation) (botagent.ToolResult, error) {
			tools++
			if invocation.OperationID == "" || invocation.Lease.RunID != lease.RunID || invocation.Call.Arguments != arguments {
				t.Error("工具运行归属或原始参数改变")
			}
			return botagent.ToolResult{Content: "Verified file contents."}, nil
		}})
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_worker_finish() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='run.finished' THEN RAISE EXCEPTION '最终事件失败'; END IF; RETURN NEW; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_worker_finish BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_worker_finish()`)
		if _, err := worker.Process(ctx, lease); err == nil {
			t.Fatal("最终事件失败被忽略")
		}
		if calls != 2 || tools != 1 {
			t.Fatal("首次工具循环次数错误", calls, tools)
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_worker_finish ON tombot.user_events`)
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 1 {
			t.Fatal("恢复失败", count, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		next := fixture.claim(t)
		run, err := worker.Process(ctx, next)
		if err != nil || run.Status != "succeeded" || calls != 2 || tools != 1 {
			t.Fatal("恢复重放模型或工具", run.Status, calls, tools, err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 24 || held != 0 {
			t.Fatal("恢复重复结算用量", known, held)
		}
	})
	t.Run("模型失联不重试且保留未知用量", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.plainBot(t))
		lease := fixture.claim(t)
		calls := 0
		worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, _ func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			return modelgateway.Result{}, errors.New("调用连接断开")
		}))
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "unknown" || calls != 1 {
			t.Fatal("失联被重试或标成功", run.Status, calls, err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 0 || held != 4352 {
			t.Fatal("未知调用释放了额度")
		}
	})
	t.Run("撤销工具权限阻止在途运行执行工具", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.toolsModel(t)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		tools := 0
		worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			spec := fixture.first.Spec
			spec.ToolPermissions = []string{}
			if _, err := fixture.catalog.Update(ctx, fixture.a, fixture.first.ID, bots.UpdateRequest{ExpectedRevision: fixture.first.Revision, Spec: spec}); err != nil {
				t.Fatal(err)
			}
			return emitTool(emit, `{"path":"notes.txt"}`)
		}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
			tools++
			return botagent.ToolResult{Content: "Should not execute."}, nil
		}})
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "failed" || run.ErrorCode != "RESOURCE_UNAVAILABLE" || tools != 0 {
			t.Fatal("撤销后仍执行工具", run.Status, tools, err)
		}
	})
	t.Run("续约保持长调用并响应用户取消", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		options := fixture.service.options
		options.LeaseLifetime = time.Second
		service, err := New(fixture.tenant, fixture.dispatch, fixture.registry, options)
		if err != nil {
			t.Fatal(err)
		}
		fixture.service = service
		submission := fixture.send(t, fixture.a, fixture.plainBot(t))
		lease := fixture.claim(t)
		entered := make(chan struct{})
		var calls atomic.Int32
		worker := fixture.worker(t, callerFunc(func(ctx context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, _ func(provider.Event) error) (modelgateway.Result, error) {
			calls.Add(1)
			close(entered)
			<-ctx.Done()
			return modelgateway.Result{}, ctx.Err()
		}))
		type outcome struct {
			run Run
			err error
		}
		finished := make(chan outcome, 1)
		go func() { run, err := worker.Process(ctx, lease); finished <- outcome{run, err} }()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("模型未启动")
		}
		time.Sleep(1100 * time.Millisecond)
		if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 0 {
			t.Fatal("续约期间运行被恢复", count, err)
		}
		if _, err = fixture.service.Cancel(ctx, fixture.a, submission.RunID); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-finished:
			if result.err != nil || result.run.Status != "canceled" || calls.Load() != 1 {
				t.Fatal("取消未收尾", result.run.Status, result.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("取消未中止模型请求")
		}
	})
	t.Run("超限和非法输出不能成为成功回复", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		bot := fixture.plainBot(t)
		for _, answer := range []string{strings.Repeat("a", 9<<10), "\x00", "   "} {
			fixture.send(t, fixture.a, bot)
			lease := fixture.claim(t)
			worker := fixture.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				return emitText(emit, answer)
			}))
			run, err := worker.Process(ctx, lease)
			if err != nil || run.Status == "succeeded" {
				t.Fatal("非法输出未明确结束", run.Status, err)
			}
		}
	})
}
