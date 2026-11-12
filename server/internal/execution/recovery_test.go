package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestExecutionFailureBoundaries(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("入队事务失败后原键重试且并发只建一次", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		request := SendRequest{ClientRequestID: ids.New(), Content: "原始消息"}
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_enqueue_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '入队事件失败'; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_enqueue_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_enqueue_event()`)
		if _, err := fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, request); err == nil {
			t.Fatal("入队事件失败被忽略")
		}
		var messages, turns, runs, jobs, sequence int
		if err := fixture.pool.QueryRow(`SELECT
			(SELECT count(*) FROM tombot.messages WHERE user_id=$1),
			(SELECT count(*) FROM tombot.turns WHERE user_id=$1),
			(SELECT count(*) FROM tombot.runs WHERE user_id=$1),
			(SELECT count(*) FROM tombot.jobs WHERE user_id=$1),
			(SELECT message_sequence FROM tombot.conversations WHERE user_id=$1 AND id=$2)`, fixture.a.UserID, fixture.first.ConversationID).Scan(&messages, &turns, &runs, &jobs, &sequence); err != nil {
			t.Fatal(err)
		}
		if messages != 0 || turns != 0 || runs != 0 || jobs != 0 || sequence != 0 {
			t.Fatal("入队失败留下部分消息、任务或序号")
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_enqueue_event ON tombot.user_events`)
		type result struct {
			submission Submission
			err        error
		}
		results := make(chan result, 8)
		var wait sync.WaitGroup
		for range 8 {
			wait.Go(func() {
				submission, err := fixture.service.Send(ctx, fixture.a, fixture.first.ConversationID, request)
				results <- result{submission, err}
			})
		}
		wait.Wait()
		close(results)
		created, runID, messageID := 0, "", ""
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.submission.IsCreated {
				created++
			}
			if runID != "" && (result.submission.RunID != runID || result.submission.MessageID != messageID) {
				t.Fatal("并发同键返回不同资源")
			}
			runID, messageID = result.submission.RunID, result.submission.MessageID
		}
		if created != 1 {
			t.Fatal("并发请求重复创建", created)
		}
	})
	t.Run("已知操作在恢复后复用且不重发", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		operation := fixture.completeModel(t, lease)
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 1 {
			t.Fatal("已知结果未进入恢复", count, err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		next := fixture.claim(t)
		work := fixture.start(t, next)
		if len(work.Operations) != 1 || work.Operations[0].ID != operation.ID || work.Selection == nil || work.Selection.Version != 1 {
			t.Fatal("恢复丢失操作或模型绑定")
		}
		replayed, created, err := fixture.service.BeginOperation(ctx, next, operationRequest("model.1"))
		if err != nil || created || replayed.ID != operation.ID || replayed.Status != "succeeded" || replayed.Attempt != 1 {
			t.Fatal("恢复将已完成操作重新执行", err)
		}
		if _, err = fixture.service.Complete(ctx, next, Outcome{Status: "succeeded", Answer: "已完成"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("恢复次数耗尽后结束且不再领取", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		submitted := fixture.send(t, fixture.a, fixture.first)
		for attempt := 1; attempt <= fixture.service.options.MaxAttempts; attempt++ {
			lease := fixture.claim(t)
			if lease.Attempt != attempt {
				t.Fatal("恢复次数错误")
			}
			fixture.start(t, lease)
			execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
			if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 1 {
				t.Fatal("失效任务未收尾", count, err)
			}
			if attempt < fixture.service.options.MaxAttempts {
				execute(t, fixture.pool, `UPDATE tombot.jobs SET available_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
			}
		}
		run, err := fixture.service.GetRun(ctx, fixture.a, submitted.RunID)
		if err != nil || run.Status != "failed" || run.ErrorCode != "RECOVERY_EXHAUSTED" {
			t.Fatal("没有执行有限恢复", err)
		}
		_, err = fixture.service.Claim(ctx, ids.New())
		expect(t, err, ErrNoWork)
		if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 0 {
			t.Fatal("终态再次恢复", count, err)
		}
	})
	t.Run("归档机器人和超限上下文明确失败", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		if _, err := fixture.catalog.SetArchived(ctx, fixture.a, fixture.first.ID, fixture.first.Revision, true); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.service.Start(ctx, lease)
		expect(t, err, ErrUnavailable)
		if _, err = fixture.service.Complete(ctx, lease, Outcome{Status: "failed", ErrorCode: "BOT_ARCHIVED"}); err != nil {
			t.Fatal("失效资源无法收尾", err)
		}
		options := fixture.service.options
		options.ContextBytes = 1024
		bounded, err := New(fixture.tenant, fixture.dispatch, fixture.registry, options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = bounded.Send(ctx, fixture.a, fixture.second.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: strings.Repeat("a", 1024)}); err != nil {
			t.Fatal(err)
		}
		next := fixture.claim(t)
		_, err = bounded.Start(ctx, next)
		expect(t, err, ErrContextLimit)
		run, err := fixture.service.GetRun(ctx, fixture.a, next.RunID)
		if err != nil || run.Status != "queued" || run.StartedAt != nil {
			t.Fatal("上下文失败后部分激活运行", err)
		}
		if _, err = bounded.Complete(ctx, next, Outcome{Status: "failed", ErrorCode: "CONTEXT_LIMIT"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("上下文截断显式标记且排除中断回复", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.start(t, lease)
		if _, err := fixture.service.Complete(ctx, lease, Outcome{Status: "failed", ErrorCode: "MODEL_FAILURE", Answer: "中断的回复"}); err != nil {
			t.Fatal(err)
		}
		submitted := fixture.send(t, fixture.a, fixture.first)
		next := fixture.claim(t)
		options := fixture.service.options
		options.ContextMessages = 2
		bounded, err := New(fixture.tenant, fixture.dispatch, fixture.registry, options)
		if err != nil {
			t.Fatal(err)
		}
		work, err := bounded.Start(ctx, next)
		if err != nil || !work.HistoryTruncated || len(work.Messages) != 1 || work.Messages[0].ID != submitted.MessageID {
			t.Fatal("上下文截断未说明或包含中断回复", err)
		}
		if _, err = bounded.BindModel(ctx, next); err != nil {
			t.Fatal(err)
		}
		options.MaxOperations = 1
		oneOperation, err := New(fixture.tenant, fixture.dispatch, fixture.registry, options)
		if err != nil {
			t.Fatal(err)
		}
		operation, _, err := oneOperation.BeginOperation(ctx, next, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = oneOperation.RecordOperation(ctx, next, operation.ID, OperationResult{Status: "failed", ErrorCode: "MODEL_REJECTED", Result: json.RawMessage(`{"usage":null}`)}); err != nil {
			t.Fatal(err)
		}
		_, _, err = oneOperation.BeginOperation(ctx, next, operationRequest("model.2"))
		expect(t, err, ErrOperationLimit)
	})
}
