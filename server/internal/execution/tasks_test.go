package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/scheduledtasks"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func taskDefinition() scheduledtasks.Definition {
	return scheduledtasks.Definition{Name: "日报", Prompt: "整理今天工作并给出摘要", Cron: "0 18 * * *", Timezone: "Asia/Shanghai", IsEnabled: true}
}
func (f fixture) saveTask(t *testing.T, definition scheduledtasks.Definition) scheduledtasks.Task {
	t.Helper()
	var task scheduledtasks.Task
	err := f.tenant.Tenant(context.Background(), f.a.UserID, func(tx *sql.Tx) error {
		var err error
		task, err = scheduledtasks.Save(context.Background(), tx, f.a.UserID, f.first.ID, "", 0, definition)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func (f fixture) dueTask(t *testing.T, task scheduledtasks.Task) {
	t.Helper()
	execute(t, f.pool, `UPDATE tombot.scheduled_tasks SET next_run_at=clock_timestamp()-interval '2 minutes' WHERE user_id=$1 AND id=$2`, f.a.UserID, task.ID)
	execute(t, f.pool, `UPDATE tombot.scheduled_task_due SET next_run_at=(SELECT next_run_at FROM tombot.scheduled_tasks WHERE user_id=$1 AND id=$2) WHERE user_id=$1 AND task_id=$2`, f.a.UserID, task.ID)
}
func (f fixture) taskCall(t *testing.T, lease Lease, name string, arguments any) (botagent.ToolResult, error) {
	t.Helper()
	call := botagent.ToolCall{ID: ids.New(), Name: name, Arguments: memoryArguments(t, arguments)}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	op, _, err := f.service.BeginOperation(context.Background(), lease, OperationRequest{StepKey: "task." + strings.ToLower(call.ID), Kind: "tool", ToolName: name, RequestDigest: digest[:]})
	if err != nil {
		return botagent.ToolResult{}, err
	}
	for _, schema := range scheduledtasks.Schemas() {
		if schema.Name == name {
			result, err := (taskTool{executor: f.service, schema: schema, maximum: 8 << 10}).Execute(context.Background(), Invocation{Lease: lease, OperationID: op.ID, Call: call})
			if err == nil {
				status, code := "succeeded", ""
				if result.IsError {
					status, code = "failed", "TOOL_FAILED"
				}
				_, err = f.service.RecordOperation(context.Background(), lease, op.ID, OperationResult{Status: status, ErrorCode: code, Result: json.RawMessage(memoryArguments(t, result))})
			}
			return result, err
		}
	}
	t.Fatal("工具未注册")
	return botagent.ToolResult{}, ErrInvalidInput
}
func taskArguments(t *testing.T, definition scheduledtasks.Definition, source string) map[string]any {
	t.Helper()
	var arguments map[string]any
	if err := json.Unmarshal([]byte(memoryArguments(t, definition)), &arguments); err != nil {
		t.Fatal(err)
	}
	arguments["source_message_id"] = source
	arguments["evidence"] = "每天整理日报"
	return arguments
}
func TestScheduledTaskConversationLifecycle(t *testing.T) {
	cluster := pgtest.Start(t)
	f := newFixture(t, cluster)
	f.toolsModel(t)
	f.service.options.MaxOperations = 32
	ctx := context.Background()
	sent, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "每天整理日报，之后可以修改或删除。"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var task scheduledtasks.Task
	worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		switch calls {
		case 1:
			if !strings.Contains(input.System, scheduledtasks.Instructions) {
				t.Error("缺少任务指引")
			}
			return emitMemory(emit, "task_create", memoryArguments(t, taskArguments(t, taskDefinition(), sent.MessageID)))
		case 2:
			result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
			if result == nil || result.IsError || json.Unmarshal([]byte(result.Content), &task) != nil || task.ID == "" {
				t.Fatal("创建未回传", result)
			}
			return emitText(emit, "已安排每天十八点整理日报。")
		}
		t.Error("重放模型调用")
		return emitText(emit, "异常")
	}), testTool{})
	lease := f.claim(t)
	execute(t, f.pool, `CREATE FUNCTION tombot.reject_task_finish() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='run.finished' THEN RAISE EXCEPTION '收尾失败'; END IF; RETURN NEW; END $body$`)
	execute(t, f.pool, `CREATE TRIGGER reject_task_finish BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_task_finish()`)
	if _, err = worker.Process(ctx, lease); err == nil {
		t.Fatal("未传播收尾失败")
	}
	execute(t, f.pool, `DROP TRIGGER reject_task_finish ON tombot.user_events`)
	execute(t, f.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	if count, err := f.service.Recover(ctx, 1); err != nil || count != 1 {
		t.Fatal("恢复失败", count, err)
	}
	execute(t, f.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
	if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" || calls != 2 {
		t.Fatal("任务创建被重放", run.Status, calls, err)
	}
	page, err := f.service.Tasks(ctx, f.a, f.first.ID, "")
	if err != nil || len(page.Tasks) != 1 {
		t.Fatal("任务未持久保存", page, err)
	}
	// 新一轮显式请求允许修改和删除，旧回合来源、外猫咪和伪造正文均不允许变更。
	update, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "每天整理日报改为十九点，然后删除这个任务。"})
	if err != nil {
		t.Fatal(err)
	}
	lease = f.claim(t)
	f.start(t, lease)
	args := taskArguments(t, taskDefinition(), sent.MessageID)
	args["id"] = task.ID
	args["expected_revision"] = task.Revision
	args["cron"] = "0 19 * * *"
	if result, err := f.taskCall(t, lease, "task_update", args); err != nil || !result.IsError {
		t.Fatal("旧来源修改了任务", result, err)
	}
	args["source_message_id"] = update.MessageID
	if result, err := f.taskCall(t, lease, "task_update", args); err != nil || result.IsError {
		t.Fatal("对话修改失败", result, err)
	}
	if result, err := f.taskCall(t, lease, "task_update", args); err != nil || !result.IsError || !strings.Contains(result.Content, "TASK_CONFLICT") {
		t.Fatal("旧版本覆盖任务", result, err)
	}
	if _, err = f.service.Tasks(ctx, f.b, f.first.ID, ""); !errors.Is(err, scheduledtasks.ErrNotFound) {
		t.Fatal("跨用户读取任务", err)
	}
	if page, err = f.service.Tasks(ctx, f.a, f.second.ID, ""); err != nil || len(page.Tasks) != 0 {
		t.Fatal("跨猫咪泄漏任务", err)
	}
	if _, err = f.service.SetTaskEnabled(ctx, f.a, f.second.ID, task.ID, 2, false); !errors.Is(err, scheduledtasks.ErrNotFound) {
		t.Fatal("跨猫咪修改任务", err)
	}
	disabled, err := f.service.SetTaskEnabled(ctx, f.a, f.first.ID, task.ID, 2, false)
	if err != nil || disabled.IsEnabled || disabled.NextRunAt != nil {
		t.Fatal("面板禁用失败", err)
	}
	enabled, err := f.service.SetTaskEnabled(ctx, f.a, f.first.ID, task.ID, disabled.Revision, true)
	if err != nil || !enabled.IsEnabled {
		t.Fatal("面板启用失败", err)
	}
	deletion := map[string]any{"id": task.ID, "expected_revision": enabled.Revision, "source_message_id": update.MessageID, "evidence": "删除这个任务"}
	if result, err := f.taskCall(t, lease, "task_delete", deletion); err != nil || result.IsError {
		t.Fatal("对话删除失败", result, err)
	}
	if page, err = f.service.Tasks(ctx, f.a, f.first.ID, ""); err != nil || len(page.Tasks) != 0 {
		t.Fatal("已删除任务仍可见", err)
	}
}

func TestScheduledTaskDispatch(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("并发抢占与重启补跑只入队一次", func(t *testing.T) {
		f := newFixture(t, cluster)
		task := f.saveTask(t, taskDefinition())
		f.dueTask(t, task)
		var group sync.WaitGroup
		counts := make(chan int, 4)
		failures := make(chan error, 4)
		for range 4 {
			group.Go(func() { count, err := f.service.DispatchTasks(ctx, 100); counts <- count; failures <- err })
		}
		group.Wait()
		close(counts)
		close(failures)
		total := 0
		for count := range counts {
			total += count
		}
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != 1 {
			t.Fatal("重复抢占", total)
		}
		rebuilt, err := New(f.tenant, f.dispatch, f.registry, f.service.options)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := rebuilt.DispatchTasks(ctx, 100); err != nil || count != 0 {
			t.Fatal("重启重放了调度", count, err)
		}
		lease := f.claim(t)
		work := f.start(t, lease)
		if len(work.Messages) != 1 || work.Messages[0].SenderKind != "system" || !strings.Contains(work.Messages[0].Blocks[0].Text, task.Prompt) {
			t.Fatal("任务未进入正确对话")
		}
		if message := messageForModel(work.Messages[0]); message.Role != "user" || !strings.Contains(message.Blocks[0].Text, "Scheduled task invocation") {
			t.Fatal("任务被提升为系统授权")
		}
		var source string
		if err = f.pool.QueryRow(`SELECT source FROM tombot.turns WHERE user_id=$1 AND id=$2`, lease.UserID, lease.TurnID).Scan(&source); err != nil || source != "routine" {
			t.Fatal("运行来源错误", source, err)
		}
		f.toolsModel(t)
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			return emitText(emit, "日报已完成")
		}), testTool{})
		if run, processErr := worker.Process(ctx, lease); processErr != nil || run.Status != "succeeded" {
			err = processErr
			t.Fatal(err)
		}
		page, err := rebuilt.Tasks(ctx, f.a, f.first.ID, "")
		if err != nil || page.Tasks[0].LastStatus != "succeeded" || page.Tasks[0].LastRunID == nil {
			t.Fatal("任务执行结果未关联", page, err)
		}
		execute(t, f.pool, `CREATE FUNCTION tombot.reject_delivery_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='message.created' THEN RAISE EXCEPTION '投递事件失败'; END IF; RETURN NEW; END $body$`)
		execute(t, f.pool, `CREATE TRIGGER reject_delivery_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_delivery_event()`)
		if count, err := rebuilt.DeliverTaskResults(ctx, 100); err == nil || count != 0 {
			t.Fatal("投递未与事件共同回滚", count, err)
		}
		if messages, err := rebuilt.Messages(ctx, f.a, f.first.ConversationID, 0, 10); err != nil || len(messages) != 0 {
			t.Fatal("失败投递遗留了消息", err)
		}
		execute(t, f.pool, `DROP TRIGGER reject_delivery_event ON tombot.user_events`)
		if count, err := rebuilt.DeliverTaskResults(ctx, 100); err != nil || count != 1 {
			t.Fatal("空闲时结果未投递", count, err)
		}
		messages, err := rebuilt.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || !strings.Contains(messages[0].Blocks[0].Text, "日报已完成") {
			t.Fatal("结果未回到对话", err)
		}
	})
	t.Run("一次性任务结束并拒绝重复启用", func(t *testing.T) {
		f := newFixture(t, cluster)
		definition := taskDefinition()
		future := time.Now().Add(time.Hour)
		definition.Cron = ""
		definition.RunAt = &future
		task := f.saveTask(t, definition)
		f.dueTask(t, task)
		if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		page, err := f.service.Tasks(ctx, f.a, f.first.ID, "")
		if err != nil || page.Tasks[0].IsEnabled || page.Tasks[0].NextRunAt != nil {
			t.Fatal("一次性任务仍在调度", err)
		}
		// 到期时间以数据库记录为准，执行后不能用过期日期重新开启。
		execute(t, f.pool, `UPDATE tombot.scheduled_tasks SET run_at=clock_timestamp()-interval '1 minute' WHERE user_id=$1 AND id=$2`, f.a.UserID, task.ID)
		if _, err = f.service.SetTaskEnabled(ctx, f.a, f.first.ID, task.ID, page.Tasks[0].Revision, true); !errors.Is(err, scheduledtasks.ErrInput) {
			t.Fatal("过期任务可重启", err)
		}
	})

	t.Run("聊天忙碌时任务独立执行且结果持久等待", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		chat := f.send(t, f.a, f.first)
		entered := make(chan struct{})
		release := make(chan struct{})
		chatResult := make(chan error, 1)
		worker := f.worker(t, callerFunc(func(ctx context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			scheduled := false
			for _, message := range input.Messages {
				for _, block := range message.Blocks {
					if strings.Contains(block.Text, "Scheduled task invocation") {
						scheduled = true
					}
				}
			}
			if scheduled {
				return emitText(emit, "后台任务已经完成")
			}
			close(entered)
			select {
			case <-release:
				return emitText(emit, "当前聊天已结束")
			case <-ctx.Done():
				return modelgateway.Result{}, ctx.Err()
			}
		}), testTool{})
		foreground := f.claim(t)
		go func() {
			run, err := worker.Process(ctx, foreground)
			if err == nil && run.Status != "succeeded" {
				err = errors.New("前台聊天执行失败")
			}
			chatResult <- err
		}()
		released := false
		defer func() {
			if !released {
				close(release)
				<-chatResult
			}
		}()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("前台未进入模型调用")
		}
		task := f.saveTask(t, taskDefinition())
		f.dueTask(t, task)
		if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 1 {
			t.Fatal("聊天忙碌阻止任务受理", count, err)
		}
		background := f.claim(t)
		if background.RunID == chat.RunID || background.ConversationID == foreground.ConversationID {
			t.Fatal("未建立独立后台运行")
		}
		if run, err := worker.Process(ctx, background); err != nil || run.Status != "succeeded" {
			t.Fatal("任务没有在前台结束前完成", run.Status, err)
		}
		var desktops int
		if err := f.pool.QueryRow(`SELECT count(DISTINCT desktop_id) FROM tombot.computer_sessions WHERE user_id=$1`, f.a.UserID).Scan(&desktops); err != nil || desktops != 2 {
			t.Fatal("任务与聊天共用了独占桌面", desktops, err)
		}
		page, err := f.service.Tasks(ctx, f.a, f.first.ID, "")
		if err != nil || page.Tasks[0].LastStatus != "succeeded" || page.Tasks[0].LastDeliveryStatus != "waiting" {
			t.Fatal("结果未等待投递", page, err)
		}
		if count, err := f.service.DeliverTaskResults(ctx, 100); err != nil || count != 0 {
			t.Fatal("结果插入了活跃聊天", count, err)
		}
		messages, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 1 || messages[0].ID != chat.MessageID {
			t.Fatal("后台内容污染了聊天", err)
		}
		close(release)
		released = true
		if err := <-chatResult; err != nil {
			t.Fatal(err)
		}
		rebuilt, err := New(f.tenant, f.dispatch, f.registry, f.service.options)
		if err != nil {
			t.Fatal(err)
		}
		var group sync.WaitGroup
		counts := make(chan int, 2)
		failures := make(chan error, 2)
		for range 2 {
			group.Go(func() { count, err := rebuilt.DeliverTaskResults(ctx, 100); counts <- count; failures <- err })
		}
		group.Wait()
		close(counts)
		close(failures)
		total := 0
		for count := range counts {
			total += count
		}
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		if total != 1 {
			t.Fatal("重启后结果丢失或重复投递", total)
		}
		messages, err = rebuilt.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 3 || !messages[0].Blocks[0].TaskResult || !strings.Contains(messages[0].Blocks[0].Text, "后台任务已经完成") || messages[1].Blocks[0].Text != "当前聊天已结束" {
			t.Fatal("结果投递顺序错误", messages, err)
		}
		if model := messageForModel(messages[0]); strings.Contains(model.Blocks[0].Text, "Scheduled task invocation") || !strings.Contains(model.Blocks[0].Text, "reference data") {
			t.Fatal("任务结果被视为新任务")
		}
	})

	t.Run("事件失败回滚入队和时间推进", func(t *testing.T) {
		f := newFixture(t, cluster)
		task := f.saveTask(t, taskDefinition())
		f.dueTask(t, task)
		execute(t, f.pool, `CREATE FUNCTION tombot.reject_task_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='task.updated' THEN RAISE EXCEPTION '事件失败'; END IF; RETURN NEW; END $body$`)
		execute(t, f.pool, `CREATE TRIGGER reject_task_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_task_event()`)
		if _, err := f.service.DispatchTasks(ctx, 100); err == nil {
			t.Fatal("忽略了事件失败")
		}
		var count int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.jobs WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil || count != 0 {
			t.Fatal("回滚遗留执行", count, err)
		}
		execute(t, f.pool, `DROP TRIGGER reject_task_event ON tombot.user_events`)
		if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 1 {
			t.Fatal("回滚后不能恢复", count, err)
		}
	})
	t.Run("删除和归档取消未来调度", func(t *testing.T) {
		f := newFixture(t, cluster)
		task := f.saveTask(t, taskDefinition())
		f.dueTask(t, task)
		if err := f.service.DeleteTask(ctx, f.a, f.first.ID, task.ID, task.Revision); err != nil {
			t.Fatal(err)
		}
		if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 0 {
			t.Fatal("删除任务仍被调度", count, err)
		}
		task = f.saveTask(t, taskDefinition())
		f.dueTask(t, task)
		if _, err := f.catalog.SetArchived(ctx, f.a, f.first.ID, f.first.Revision, true); err != nil {
			t.Fatal(err)
		}
		if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		page, err := f.service.Tasks(ctx, f.a, f.first.ID, "")
		if err != nil || page.Tasks[0].IsEnabled || page.Tasks[0].LastError != "TARGET_UNAVAILABLE" {
			t.Fatal("归档仍执行任务", page, err)
		}
	})
}

// 配额与分页必须绑定猫咪；调度角色仅能读取无正文索引。
func TestTaskBoundaries(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	ctx := context.Background()
	for range scheduledtasks.MaxTasks {
		f.saveTask(t, taskDefinition())
	}
	err := f.tenant.Tenant(ctx, f.a.UserID, func(tx *sql.Tx) error {
		_, err := scheduledtasks.Save(ctx, tx, f.a.UserID, f.first.ID, "", 0, taskDefinition())
		return err
	})
	if !errors.Is(err, scheduledtasks.ErrCapacity) {
		t.Fatal("未限制猫咪任务数量", err)
	}
	seen := map[string]bool{}
	before := ""
	for {
		page, err := f.service.Tasks(ctx, f.a, f.first.ID, before)
		if err != nil || len(page.Tasks) > scheduledtasks.PageSize {
			t.Fatal("分页失败", err)
		}
		for _, task := range page.Tasks {
			if seen[task.ID] {
				t.Fatal("分页重复")
			}
			seen[task.ID] = true
		}
		if page.NextBefore == "" {
			break
		}
		before = page.NextBefore
	}
	if len(seen) != scheduledtasks.MaxTasks {
		t.Fatal("分页漏项", len(seen))
	}
	if _, err := f.service.Tasks(ctx, f.a, f.first.ID, "invalid"); !errors.Is(err, scheduledtasks.ErrInput) {
		t.Fatal("接受了非法游标", err)
	}
	for _, table := range []string{"scheduled_tasks", "scheduled_task_runs"} {
		if err := f.dispatch.Platform(ctx, func(tx *sql.Tx) error {
			var count int
			return tx.QueryRowContext(ctx, "SELECT count(*) FROM tombot."+table).Scan(&count)
		}); err == nil {
			t.Fatal("调度角色越权读取正文", table)
		}
	}
	if _, err := f.pool.Exec(`INSERT INTO tombot.conversations(user_id,id,kind,bot_id,title,last_read_sequence) VALUES($1,$2,'task',$3,'非法已读位置',1)`, f.a.UserID, ids.New(), f.first.ID); err == nil {
		t.Fatal("迁移移除了已读序号约束")
	}
}

func TestFailedTaskResultDelivery(t *testing.T) {
	f := newFixture(t, pgtest.Start(t))
	ctx := context.Background()
	task := f.saveTask(t, taskDefinition())
	f.dueTask(t, task)
	if count, err := f.service.DispatchTasks(ctx, 100); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	lease := f.claim(t)
	f.start(t, lease)
	if _, err := f.service.Complete(ctx, lease, Outcome{Status: "failed", ErrorCode: "MODEL_UNAVAILABLE"}); err != nil {
		t.Fatal(err)
	}
	archived, err := f.catalog.SetArchived(ctx, f.a, f.first.ID, f.first.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := f.service.DeliverTaskResults(ctx, 100); err != nil || count != 0 {
		t.Fatal("向归档聊天投递了结果", count, err)
	}
	var delayed bool
	if err := f.pool.QueryRow(`SELECT available_at>clock_timestamp() FROM tombot.scheduled_task_delivery WHERE user_id=$1 AND run_id=$2`, f.a.UserID, lease.RunID).Scan(&delayed); err != nil || !delayed {
		t.Fatal("不可用聊天未让出投递批次", err)
	}
	if _, err := f.catalog.SetArchived(ctx, f.a, f.first.ID, archived.Revision, false); err != nil {
		t.Fatal(err)
	}
	execute(t, f.pool, `UPDATE tombot.scheduled_task_delivery SET available_at=clock_timestamp() WHERE user_id=$1 AND run_id=$2`, f.a.UserID, lease.RunID)
	if count, err := f.service.DeliverTaskResults(ctx, 100); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	messages, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Blocks[0].Text, "未完成") {
		t.Fatal("失败任务未明确告知用户", err)
	}
	if count, err := f.service.DeliverTaskResults(ctx, 100); err != nil || count != 0 {
		t.Fatal("重复投递失败通知", count, err)
	}
	result := messages[0]
	if _, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "请解释失败原因", ReplyToID: result.ID}); err != nil {
		t.Fatal("任务结果不能引用", err)
	}
	if _, err := f.service.SetReaction(ctx, f.a, result.ID, ReactionRequest{ClientRequestID: ids.New(), Emoji: "👍", IsActive: true}); err != nil {
		t.Fatal("任务结果不能标记表情", err)
	}
	work := f.start(t, f.claim(t))
	found := false
	for _, message := range work.Messages {
		if message.ReplyTo != nil && message.ReplyTo.MessageID == result.ID {
			found = true
			if model := messageForModel(message); !strings.Contains(model.Blocks[0].Text, "未完成") {
				t.Fatal("任务结果引用丢失上下文")
			}
		}
	}
	if !found {
		t.Fatal("模型没有收到对结果的反馈")
	}
}
