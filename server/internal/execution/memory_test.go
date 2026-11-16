package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botmemory"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func memoryArguments(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestMemoryMutationBoundaries(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	prepare := func(t *testing.T) (fixture, Lease, Submission) {
		f := newFixture(t, cluster)
		sent, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: preference("").Evidence})
		if err != nil {
			t.Fatal(err)
		}
		lease := f.claim(t)
		f.start(t, lease)
		return f, lease, sent
	}
	t.Run("回执失败原子回滚记忆", func(t *testing.T) {
		f, lease, sent := prepare(t)
		execute(t, f.pool, `CREATE FUNCTION tombot.reject_memory_receipt() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='tool' AND NEW.status='succeeded' THEN RAISE EXCEPTION '回执写入失败'; END IF; RETURN NEW; END $body$`)
		execute(t, f.pool, `CREATE TRIGGER reject_memory_receipt BEFORE UPDATE ON tombot.run_operations FOR EACH ROW EXECUTE FUNCTION tombot.reject_memory_receipt()`)
		if result, err := f.memoryCall(t, lease, "memory_write", preference(sent.MessageID)); err == nil {
			t.Fatal("记账失败仍报告成功", result)
		}
		var count int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2`, f.a.UserID, f.first.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("回执失败留下了记忆", count, err)
		}
	})
	t.Run("版本竞争与强制行级隔离", func(t *testing.T) {
		f, lease, sent := prepare(t)
		change := preference(sent.MessageID)
		if result, err := f.memoryCall(t, lease, "memory_write", change); err != nil || result.IsError {
			t.Fatal("保存失败", result, err)
		}
		change.ExpectedRevision = 1
		results := make(chan error, 2)
		var group sync.WaitGroup
		for range 2 {
			group.Add(1)
			go func() {
				defer group.Done()
				results <- f.tenant.Tenant(ctx, f.a.UserID, func(tx *sql.Tx) error {
					_, err := botmemory.Apply(ctx, tx, f.a.UserID, f.first.ID, change, botmemory.Source{MessageID: sent.MessageID, Sequence: 1}, false)
					return err
				})
			}()
		}
		group.Wait()
		close(results)
		succeeded, conflicted := 0, 0
		for err := range results {
			if err == nil {
				succeeded++
			} else if errors.Is(err, botmemory.ErrConflict) {
				conflicted++
			} else {
				t.Fatal(err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatal("并发版本控制失败", succeeded, conflicted)
		}
		if err := f.tenant.Tenant(ctx, f.b.UserID, func(tx *sql.Tx) error {
			// 故意使用另一租户的查询条件，验证隔离依赖数据库策略而非调用方自觉过滤。
			page, err := botmemory.Search(ctx, tx, f.a.UserID, f.first.ID, botmemory.Query{})
			if err == nil && len(page.Entries) != 0 {
				t.Error("强制行级隔离失效")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("全部遗忘后仅新原话可重新学习", func(t *testing.T) {
		f, lease, sent := prepare(t)
		if result, err := f.memoryCall(t, lease, "memory_write", preference(sent.MessageID)); err != nil || result.IsError {
			t.Fatal(result, err)
		}
		forget, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "忘记全部记忆"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.mergeRunInputs(ctx, lease, "model.1", modelgateway.Input{}); err != nil {
			t.Fatal(err)
		}
		if result, err := f.memoryCall(t, lease, "memory_delete", map[string]any{"key": "*", "expected_revision": 0, "source_message_id": forget.MessageID, "evidence": "忘记全部记忆"}); err != nil || result.IsError {
			t.Fatal(result, err)
		}
		fresh, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "重新记住：" + preference("").Evidence})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.mergeRunInputs(ctx, lease, "model.2", modelgateway.Input{}); err != nil {
			t.Fatal(err)
		}
		if result, err := f.memoryCall(t, lease, "memory_write", preference(fresh.MessageID)); err != nil || result.IsError {
			t.Fatal("新原话未能形成记忆", result, err)
		}
	})
}

func emitMemory(emit func(provider.Event) error, name, arguments string) (modelgateway.Result, error) {
	for _, event := range []provider.Event{{Kind: "tool_use_start", Index: 0, ToolCallID: "memory-call", ToolName: name}, {Kind: "tool_use_delta", Index: 0, ToolCallID: "memory-call", InputDelta: arguments}, {Kind: "tool_use_end", Index: 0, ToolCallID: "memory-call", ToolName: name, InputDelta: arguments}, {Kind: "stop", StopReason: "tool_calls"}} {
		if err := emit(event); err != nil {
			return modelgateway.Result{}, err
		}
	}
	return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
}

func (f fixture) memoryCall(t *testing.T, lease Lease, name string, arguments any) (botagent.ToolResult, error) {
	t.Helper()
	call := botagent.ToolCall{ID: ids.New(), Name: name, Arguments: memoryArguments(t, arguments)}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	op, _, err := f.service.BeginOperation(context.Background(), lease, OperationRequest{StepKey: "memory." + strings.ToLower(call.ID), Kind: "tool", ToolName: name, RequestDigest: digest[:]})
	if err != nil {
		return botagent.ToolResult{}, err
	}
	for _, schema := range botmemory.Schemas() {
		if schema.Name != name {
			continue
		}
		result, err := (memoryTool{executor: f.service, schema: schema, maximum: 8 << 10}).Execute(context.Background(), Invocation{Lease: lease, OperationID: op.ID, Call: call})
		if err == nil {
			status, code := "succeeded", ""
			if result.IsError {
				status, code = "failed", "TOOL_FAILED"
			}
			_, err = f.service.RecordOperation(context.Background(), lease, op.ID, OperationResult{Status: status, ErrorCode: code, Result: json.RawMessage(memoryArguments(t, result))})
		}
		return result, err
	}
	t.Fatal("未找到记忆工具")
	return botagent.ToolResult{}, nil
}

func preference(source string) botmemory.Change {
	return botmemory.Change{Key: "work.delivery", Category: "workflow", Summary: "先给结论再解释", Content: "工作汇报先给结论，再给原因和验证结果。", SourceMessageID: source, Evidence: "工作汇报先给结论，再给原因和验证结果"}
}

func TestMemoryWorkerPersistenceAndRecovery(t *testing.T) {
	cluster := pgtest.Start(t)
	f := newFixture(t, cluster)
	f.toolsModel(t)
	ctx := context.Background()
	submitted, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "以后工作汇报先给结论，再给原因和验证结果。"})
	if err != nil {
		t.Fatal(err)
	}
	lease := f.claim(t)
	calls := 0
	worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		switch calls {
		case 1:
			if !strings.Contains(input.System, botmemory.Instructions) || !strings.Contains(memoryArguments(t, input.Messages), submitted.MessageID) {
				t.Error("记忆指引或来源标识缺失")
			}
			return emitMemory(emit, "memory_search", `{"query":"work.delivery","offset":0}`)
		case 2:
			if result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult; result == nil || result.IsError || !strings.Contains(result.Content, `"entries":[]`) {
				t.Error("新猫咪记忆应为空")
			}
			return emitMemory(emit, "memory_write", memoryArguments(t, preference(submitted.MessageID)))
		case 3:
			if result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult; result == nil || result.IsError || !strings.Contains(result.Content, `"revision":1`) {
				t.Error("写入未成功回传")
			}
			return emitText(emit, "记住了，以后先给结论。")
		default:
			t.Error("恢复重复调用了模型")
		}
		return emitText(emit, "异常调用")
	}), testTool{})
	execute(t, f.pool, `CREATE FUNCTION tombot.reject_memory_finish() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='run.finished' THEN RAISE EXCEPTION '收尾失败'; END IF; RETURN NEW; END $body$`)
	execute(t, f.pool, `CREATE TRIGGER reject_memory_finish BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_memory_finish()`)
	if _, err = worker.Process(ctx, lease); err == nil {
		t.Fatal("收尾失败被吞掉")
	}
	if calls != 3 {
		t.Fatal("记忆未经过完整模型工具循环", calls)
	}
	if work := f.start(t, lease); work.MemoryContext != "" {
		t.Fatal("恢复时快照被新记忆改写")
	}
	execute(t, f.pool, `DROP TRIGGER reject_memory_finish ON tombot.user_events`)
	execute(t, f.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	if count, err := f.service.Recover(ctx, 1); err != nil || count != 1 {
		t.Fatal("恢复失败", count, err)
	}
	execute(t, f.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" || calls != 3 {
		t.Fatal("重放失败或重复写入", run.Status, calls, err)
	}
	var revision int64
	if err = f.pool.QueryRow(`SELECT revision FROM tombot.bot_memories WHERE user_id=$1 AND bot_id=$2`, f.a.UserID, f.first.ID).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("记忆被重复更新", err, revision)
	}
	// 缩小历史窗口并重建服务，证明检索不依赖进程缓存或原始聊天仍在模型窗口内。
	options := f.service.options
	options.ContextMessages = 1
	f.service, err = New(f.tenant, f.dispatch, f.registry, options)
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, f.a, f.first)
	reads := 0
	reopened := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		reads++
		if reads == 1 {
			if len(input.Messages) != 2 || !strings.Contains(input.Messages[0].Blocks[0].Text, "先给结论") || strings.Contains(input.Messages[1].Blocks[0].Text, "以后工作汇报") {
				t.Error("重启后的长期记忆未脱离历史窗口保存")
			}
			return emitMemory(emit, "memory_search", `{"query":"结论","offset":0}`)
		}
		if result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult; result == nil || result.IsError || !strings.Contains(result.Content, "验证结果") {
			t.Error("中文检索未恢复完整工作习惯")
		}
		return emitText(emit, "结论：检查完成。原因和验证结果如下。")
	}), testTool{})
	if run, err := reopened.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" || reads != 2 {
		t.Fatal("跨回合记忆未生效", run.Status, reads, err)
	}
}

func TestMemoryScopeSourcesAndCorrections(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("纠正遗忘和跨猫咪隔离", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.service.options.MaxOperations = 32
		initial, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: preference("").Evidence})
		if err != nil {
			t.Fatal(err)
		}
		lease := f.claim(t)
		f.start(t, lease)
		write := preference(initial.MessageID)
		result, err := f.memoryCall(t, lease, "memory_write", write)
		if err != nil || result.IsError {
			t.Fatal("首次保存失败", result, err)
		}
		result, err = f.memoryCall(t, lease, "memory_write", write)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "MEMORY_CONFLICT") {
			t.Fatal("旧版本覆盖了记忆", result, err)
		}
		updated, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "纠正一下，以后先给风险，再给结论。"})
		if err != nil {
			t.Fatal(err)
		}
		write.SourceMessageID = updated.MessageID
		write.Evidence = "以后先给风险，再给结论"
		write.Content = write.Evidence
		write.Summary = "先给风险"
		write.ExpectedRevision = 1
		result, err = f.memoryCall(t, lease, "memory_write", write)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "INVALID_SOURCE") {
			t.Fatal("尚未传给模型的消息可被引用", result, err)
		}
		if _, err = f.service.mergeRunInputs(ctx, lease, "model.1", modelgateway.Input{}); err != nil {
			t.Fatal(err)
		}
		result, err = f.memoryCall(t, lease, "memory_write", write)
		if err != nil || result.IsError || !strings.Contains(result.Content, `"revision":2`) {
			t.Fatal("纠正失败", result, err)
		}
		for _, scope := range []struct{ user, bot string }{{f.a.UserID, f.second.ID}, {f.b.UserID, f.foreign.ID}, {f.b.UserID, f.first.ID}} {
			if err = f.tenant.Tenant(ctx, scope.user, func(tx *sql.Tx) error {
				page, err := botmemory.Search(ctx, tx, scope.user, scope.bot, botmemory.Query{})
				if err == nil && len(page.Entries) != 0 {
					t.Error("跨猫咪或跨用户读取了记忆")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		forget, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "忘记我的汇报顺序。"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.mergeRunInputs(ctx, lease, "model.2", modelgateway.Input{}); err != nil {
			t.Fatal(err)
		}
		deletion := map[string]any{"key": write.Key, "expected_revision": 2, "source_message_id": forget.MessageID, "evidence": "忘记我的汇报顺序"}
		result, err = f.memoryCall(t, lease, "memory_delete", deletion)
		if err != nil || result.IsError {
			t.Fatal("遗忘失败", result, err)
		}
		result, err = f.memoryCall(t, lease, "memory_search", botmemory.Query{})
		if err != nil || strings.Contains(result.Content, "先给风险") || !strings.Contains(result.Content, `"is_deleted":true`) {
			t.Fatal("遗忘后仍返回正文", result, err)
		}
		write.ExpectedRevision = 3
		result, err = f.memoryCall(t, lease, "memory_write", write)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "INVALID_SOURCE") {
			t.Fatal("旧来源复活了已遗忘记忆", result, err)
		}
	})
	t.Run("伪造证据和来源拒绝", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.service.options.MaxOperations = 32
		own := f.send(t, f.a, f.first)
		other := f.send(t, f.a, f.second)
		foreign := f.send(t, f.b, f.foreign)
		lease := f.claim(t)
		f.start(t, lease)
		// 调度器可能先领取另一租户，只从目标运行开始验证。
		for lease.RunID != own.RunID {
			lease = f.claim(t)
			f.start(t, lease)
		}
		for _, source := range []string{own.MessageID, other.MessageID, foreign.MessageID, ids.New()} {
			change := preference(source)
			result, err := f.memoryCall(t, lease, "memory_write", change)
			if err != nil || !result.IsError || !strings.Contains(result.Content, "INVALID_SOURCE") {
				t.Fatal("无效来源被接受", result, err)
			}
		}
		for _, arguments := range []any{nil, map[string]any{"query": ""}, map[string]any{"query": "", "offset": 0, "bot_id": f.second.ID}} {
			result, err := f.memoryCall(t, lease, "memory_search", arguments)
			if err != nil || !result.IsError {
				t.Fatal("参数校验遗漏", result, err)
			}
		}
		stale := lease
		stale.FencingToken++
		if _, err := f.memoryCall(t, stale, "memory_search", botmemory.Query{}); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("旧租约可读记忆", err)
		}
	})
	t.Run("容量分页和全部遗忘", func(t *testing.T) {
		f := newFixture(t, cluster)
		sent, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: preference("").Evidence})
		if err != nil {
			t.Fatal(err)
		}
		lease := f.claim(t)
		f.start(t, lease)
		for i := 0; i < botmemory.MaxEntries; i++ {
			execute(t, f.pool, `INSERT INTO tombot.bot_memories(user_id,bot_id,key,category,summary,content,revision,source_message_id,source_sequence) VALUES($1,$2,$3,'habit',$4,$4,1,$5,1)`, f.a.UserID, f.first.ID, fmt.Sprintf("habit.%03d", i), strings.Repeat("习惯", 18), sent.MessageID)
		}
		result, err := f.memoryCall(t, lease, "memory_write", preference(sent.MessageID))
		if err != nil || !result.IsError || !strings.Contains(result.Content, "MEMORY_CAPACITY") {
			t.Fatal("容量限制失效", result, err)
		}
		result, err = f.memoryCall(t, lease, "memory_search", botmemory.Query{})
		var page botmemory.Page
		if err != nil || json.Unmarshal([]byte(result.Content), &page) != nil || len(page.Entries) != botmemory.PageSize || page.NextOffset == nil || *page.NextOffset != botmemory.PageSize {
			t.Fatal("分页失败", result, err)
		}
		if err = f.tenant.Tenant(ctx, f.a.UserID, func(tx *sql.Tx) error {
			snapshot, err := botmemory.Snapshot(ctx, tx, f.a.UserID, f.first.ID, 1024)
			if err == nil && (len(snapshot) > 1024 || !strings.Contains(snapshot, `"truncated":true`)) {
				t.Error("摘要预算无效")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		forget, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "忘记全部长期记忆"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.mergeRunInputs(ctx, lease, "model.1", modelgateway.Input{}); err != nil {
			t.Fatal(err)
		}
		result, err = f.memoryCall(t, lease, "memory_delete", map[string]any{"key": "*", "expected_revision": 0, "source_message_id": forget.MessageID, "evidence": "忘记全部长期记忆"})
		if err != nil || result.IsError || !strings.Contains(result.Content, `"forgotten":256`) {
			t.Fatal("全部遗忘失败", result, err)
		}
		result, err = f.memoryCall(t, lease, "memory_write", preference(sent.MessageID))
		if err != nil || !result.IsError || !strings.Contains(result.Content, "INVALID_SOURCE") {
			t.Fatal("全部遗忘后旧来源仍能写入新键", result, err)
		}
	})
}
