package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/modelusage"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func counter(value int) *int  { return &value }
func rate(value int64) *int64 { return &value }
func reported(input, output int) *provider.ReportedUsage {
	return &provider.ReportedUsage{InputTokens: counter(input), OutputTokens: counter(output), CacheReadTokens: counter(0), InputIncludesCache: true}
}
func (fixture fixture) prepareModel(t *testing.T, lease Lease) {
	t.Helper()
	fixture.start(t, lease)
	if _, err := fixture.service.BindModel(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}
func (fixture fixture) totals(t *testing.T, userID string) (int64, int64) {
	t.Helper()
	var known, held int64
	if err := fixture.pool.QueryRow(`SELECT COALESCE(sum(known_tokens),0),COALESCE(sum(held_tokens),0) FROM tombot.usage_periods WHERE user_id=$1`, userID).Scan(&known, &held); err != nil {
		t.Fatal(err)
	}
	return known, held
}
func (fixture fixture) recordMeasured(t *testing.T, lease Lease, operation Operation, usage *provider.ReportedUsage) {
	t.Helper()
	if _, err := fixture.service.RecordOperation(context.Background(), lease, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{"answer":"已完成"}`), Usage: usage, UsageFinal: true}); err != nil {
		t.Fatal(err)
	}
}

func TestModelAdmissionAndUsage(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("并行预留不突破月度额度且重复不计两次", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET monthly_token_limit=4352 WHERE user_id=$1`, fixture.a.UserID)
		fixture.send(t, fixture.a, fixture.first)
		fixture.send(t, fixture.a, fixture.second)
		first, second := fixture.claim(t), fixture.claim(t)
		fixture.prepareModel(t, first)
		fixture.prepareModel(t, second)
		type result struct {
			lease     Lease
			operation Operation
			err       error
		}
		results := make(chan result, 2)
		var wait sync.WaitGroup
		for _, lease := range []Lease{first, second} {
			wait.Go(func() {
				operation, _, err := fixture.service.BeginOperation(ctx, lease, operationRequest("model.1"))
				results <- result{lease, operation, err}
			})
		}
		wait.Wait()
		close(results)
		var winner, loser result
		for result := range results {
			if result.err == nil {
				if winner.operation.ID != "" {
					t.Fatal("并发突破额度")
				}
				winner = result
			} else {
				expect(t, result.err, modelusage.ErrQuota)
				loser = result
			}
		}
		if winner.operation.ID == "" || loser.lease.JobID == "" {
			t.Fatal("未得到一成功一拒绝")
		}
		known, held := fixture.totals(t, fixture.a.UserID)
		if known != 0 || held != 4352 {
			t.Fatal("预留被当作用量或未生效", known, held)
		}
		replay, created, err := fixture.service.BeginOperation(ctx, winner.lease, operationRequest("model.1"))
		if err != nil || created || replay.ID != winner.operation.ID {
			t.Fatal("重复操作重新预留", err)
		}
		fixture.recordMeasured(t, winner.lease, winner.operation, reported(10, 5))
		fixture.recordMeasured(t, winner.lease, winner.operation, reported(10, 5))
		known, held = fixture.totals(t, fixture.a.UserID)
		if known != 15 || held != 0 {
			t.Fatal("未释放差额或重复计费", known, held)
		}
		_, _, err = fixture.service.BeginOperation(ctx, loser.lease, operationRequest("model.1"))
		expect(t, err, modelusage.ErrQuota)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET monthly_token_limit=5000 WHERE user_id=$1`, fixture.a.UserID)
		if _, created, err = fixture.service.BeginOperation(ctx, loser.lease, operationRequest("model.1")); err != nil || !created {
			t.Fatal("拒绝请求留下操作记录", err)
		}
	})
	t.Run("并发名额按用户隔离且未知保留额度", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		execute(t, fixture.pool, `UPDATE tombot.user_limits SET concurrent_model_calls=1,monthly_token_limit=NULL WHERE user_id=$1`, fixture.a.UserID)
		fixture.send(t, fixture.a, fixture.first)
		fixture.send(t, fixture.a, fixture.second)
		first, second := fixture.claim(t), fixture.claim(t)
		fixture.prepareModel(t, first)
		fixture.prepareModel(t, second)
		operation, _, err := fixture.service.BeginOperation(ctx, first, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = fixture.service.BeginOperation(ctx, second, operationRequest("model.1"))
		expect(t, err, modelusage.ErrConcurrency)
		fixture.send(t, fixture.b, fixture.foreign)
		foreign := fixture.claim(t)
		fixture.prepareModel(t, foreign)
		if _, _, err = fixture.service.BeginOperation(ctx, foreign, operationRequest("model.1")); err != nil {
			t.Fatal("另一用户被占用名额阻塞", err)
		}
		if _, err = fixture.service.RecordOperation(ctx, first, operation.ID, OperationResult{Status: "unknown", ErrorCode: "MODEL_DISCONNECTED", Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		known, held := fixture.totals(t, fixture.a.UserID)
		if known != 0 || held != 4352 {
			t.Fatal("未知用量被当成免费调用", known, held)
		}
		if _, _, err = fixture.service.BeginOperation(ctx, second, operationRequest("model.1")); err != nil {
			t.Fatal("本地请求结束后未释放并发名额", err)
		}
	})
	t.Run("调用硬超时后保留额度且迟到证据不覆盖运行", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		submitted := fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.prepareModel(t, lease)
		request := operationRequest("model.1")
		request.Model.Lifetime = time.Second
		operation, _, err := fixture.service.BeginOperation(ctx, lease, request)
		if err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err := fixture.service.Recover(ctx, 1); err != nil || count != 1 {
			t.Fatal("运行恢复失败", count, err)
		}
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 0 {
			t.Fatal("用运行过期代替调用硬时限", count, err)
		}
		time.Sleep(1100 * time.Millisecond)
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 1 {
			t.Fatal("调用超时未对账", count, err)
		}
		known, held := fixture.totals(t, lease.UserID)
		if known != 0 || held != 4352 {
			t.Fatal("失联预留被清零", known, held)
		}
		evidence := ids.New()
		observation := modelusage.Observation{Usage: reported(20, 3), Final: true}
		for range 2 {
			if err = fixture.service.AppendModelEvidence(ctx, lease, operation.ID, evidence, observation); err != nil {
				t.Fatal(err)
			}
		}
		known, held = fixture.totals(t, lease.UserID)
		if known != 0 || held != 4352 {
			t.Fatal("旧 Worker 直接改写汇总")
		}
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 1 {
			t.Fatal("迟到用量未对账", count, err)
		}
		known, held = fixture.totals(t, lease.UserID)
		if known != 23 || held != 0 {
			t.Fatal("迟到用量未真实结算", known, held)
		}
		run, err := fixture.service.GetRun(ctx, fixture.a, submitted.RunID)
		if err != nil || run.Status != "unknown" {
			t.Fatal("迟到证据覆盖运行终态", err)
		}
		_, err = fixture.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: "succeeded", Result: json.RawMessage(`{}`), Usage: reported(20, 3), UsageFinal: true})
		expect(t, err, ErrLeaseLost)
		expect(t, fixture.service.AppendModelEvidence(ctx, lease, operation.ID, evidence, modelusage.Observation{Usage: reported(21, 3), Final: true}), modelusage.ErrConflict)
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 0 {
			t.Fatal("重复结算迟到结果", count, err)
		}
	})
	t.Run("固定配置校验和结算事件故障共同回滚", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.prepareModel(t, lease)
		bad := operationRequest("model.1")
		bad.Model.Configuration.ContextLimit = 2048
		if _, _, err := fixture.service.BeginOperation(ctx, lease, bad); err == nil {
			t.Fatal("未拒绝更低的虚构预留上限")
		}
		if known, held := fixture.totals(t, lease.UserID); known != 0 || held != 0 {
			t.Fatal("配置失败留下额度占用")
		}
		operation, _, err := fixture.service.BeginOperation(ctx, lease, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		execute(t, fixture.pool, `CREATE FUNCTION tombot.reject_meter_event() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION '用量事件失败'; END $body$`)
		execute(t, fixture.pool, `CREATE TRIGGER reject_meter_event BEFORE INSERT ON tombot.user_events FOR EACH ROW EXECUTE FUNCTION tombot.reject_meter_event()`)
		result := OperationResult{Status: "succeeded", Result: json.RawMessage(`{"answer":"已完成"}`), Usage: reported(10, 5), UsageFinal: true}
		if _, err = fixture.service.RecordOperation(ctx, lease, operation.ID, result); err == nil {
			t.Fatal("用量事件失败被忽略")
		}
		var state string
		if err = fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE user_id=$1 AND id=$2`, lease.UserID, operation.ID).Scan(&state); err != nil || state != "started" {
			t.Fatal("操作未与用量共同回滚", err)
		}
		known, held := fixture.totals(t, lease.UserID)
		if known != 0 || held != 4352 {
			t.Fatal("用量部分结算")
		}
		execute(t, fixture.pool, `DROP TRIGGER reject_meter_event ON tombot.user_events`)
		fixture.recordMeasured(t, lease, operation, reported(10, 5))
		result.Usage = reported(11, 5)
		_, err = fixture.service.RecordOperation(ctx, lease, operation.ID, result)
		expect(t, err, modelusage.ErrConflict)
		err = fixture.dispatch.Platform(ctx, func(transaction *sql.Tx) error {
			var count int
			return transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.model_usage`).Scan(&count)
		})
		if err == nil {
			t.Fatal("调度角色读取供应商计数")
		}
		err = fixture.tenant.Tenant(ctx, fixture.b.UserID, func(transaction *sql.Tx) error {
			var count int
			if err := transaction.QueryRowContext(ctx, `SELECT count(*) FROM tombot.usage_reservations WHERE user_id=$1`, fixture.a.UserID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("跨用户用量可见")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("缺失缓存拆分的费用可以由一致证据补齐", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		prices := modelregistry.Prices{Input: rate(1_000_000), Output: rate(2_000_000), CacheRead: rate(500_000)}
		version, err := fixture.registry.CreateDraft(ctx, fixture.actor, ids.New(), modelregistry.DraftRequest{PolicyID: fixture.policyID, ModelID: fixture.modelID, OutputLimit: 256, Prices: prices})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.registry.Publish(ctx, fixture.actor, ids.New(), fixture.policyID, version.Number, fixture.revision(t)); err != nil {
			t.Fatal(err)
		}
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.prepareModel(t, lease)
		request := operationRequest("model.1")
		request.Model.Configuration.Prices = prices
		operation, _, err := fixture.service.BeginOperation(ctx, lease, request)
		if err != nil {
			t.Fatal(err)
		}
		usage := reported(10, 2)
		usage.CacheReadTokens = nil
		fixture.recordMeasured(t, lease, operation, usage)
		var cost *int64
		if err = fixture.pool.QueryRow(`SELECT cost_micro_usd FROM tombot.usage_reservations WHERE user_id=$1 AND id=$2`, lease.UserID, operation.ID).Scan(&cost); err != nil || cost != nil {
			t.Fatal("未知缓存价格被估算", err)
		}
		usage.CacheReadTokens = counter(4)
		if err = fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), modelusage.Observation{Usage: usage, Final: true}); err != nil {
			t.Fatal(err)
		}
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 1 {
			t.Fatal("费用补齐失败", count, err)
		}
		if err = fixture.pool.QueryRow(`SELECT cost_micro_usd FROM tombot.usage_reservations WHERE user_id=$1 AND id=$2`, lease.UserID, operation.ID).Scan(&cost); err != nil || cost == nil || *cost != 12 {
			t.Fatal("费用补齐不准确", err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 12 || held != 0 {
			t.Fatal("补齐费用重复计 token")
		}
		expect(t, fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), modelusage.Observation{Usage: reported(11, 1), Final: true}), modelusage.ErrConflict)
	})
	t.Run("部分计数增加保留额度且不接受倒退的最终计数", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.send(t, fixture.a, fixture.first)
		lease := fixture.claim(t)
		fixture.prepareModel(t, lease)
		operation, _, err := fixture.service.BeginOperation(ctx, lease, operationRequest("model.1"))
		if err != nil {
			t.Fatal(err)
		}
		partial := modelusage.Observation{Usage: &provider.ReportedUsage{OutputTokens: counter(20000), InputIncludesCache: true}}
		if err = fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), partial); err != nil {
			t.Fatal(err)
		}
		if err = fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), modelusage.Observation{}); err != nil {
			t.Fatal(err)
		}
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 1 {
			t.Fatal("部分计数未对账", count, err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 0 || held != 20000 {
			t.Fatal("较新的空证据覆盖已知下界", known, held)
		}
		expect(t, fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), modelusage.Observation{Usage: reported(10, 2), Final: true}), modelusage.ErrConflict)
		if err = fixture.service.AppendModelEvidence(ctx, lease, operation.ID, ids.New(), modelusage.Observation{Usage: reported(10, 20000), Final: true}); err != nil {
			t.Fatal(err)
		}
		if count, err := fixture.service.ReconcileUsage(ctx, 1); err != nil || count != 1 {
			t.Fatal("真实超额计数未结算", count, err)
		}
		if known, held := fixture.totals(t, lease.UserID); known != 20010 || held != 0 {
			t.Fatal("真实计数被预留上限截断", known, held)
		}
	})
}
