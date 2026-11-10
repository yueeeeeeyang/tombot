package usage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/usage"
)

func saveBudget(t *testing.T, db *sql.DB, limit int64, action string) {
	t.Helper()
	var patch service.SettingsPatch
	encoded, err := json.Marshal(map[string]any{"monthly_budget": map[string]any{"limit_micro": limit, "action": action, "warn_percent": 80, "revision": 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &patch); err != nil {
		t.Fatal(err)
	}
	if _, err = service.NewSettings(db).Update(context.Background(), patch, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestMonthlyConcurrentSettlementDedupAndAdmission(t *testing.T) {
	db, runs, model := chainFixture(t)
	ctx := context.Background()
	saveBudget(t, db, 20, "stop")
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			if _, err := usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 1}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	monthly, err := usage.Monthly(ctx, db, time.Now())
	if err != nil || monthly.CostMicro != 20 || monthly.Calls != 20 || monthly.Status != "limit" {
		t.Fatal("月度重复统计父子费用", monthly, err)
	}
	var alerts, notices, audits int
	for query, target := range map[string]*int{"SELECT COUNT(*) FROM monthly_budget_alerts": &alerts, "SELECT COUNT(*) FROM events WHERE kind='system.notice' AND json_extract(payload_json,'$.code')='MONTHLY_BUDGET'": &notices, "SELECT COUNT(*) FROM audit_log WHERE action='usage.budget_alert'": &audits} {
		if err = db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if alerts != 2 || notices != 2 || audits != 2 {
		t.Fatal("并发提醒未去重", alerts, notices, audits)
	}
	err = store.Transact(ctx, db, func(tx *sql.Tx) error { return usage.CheckMonthlyAdmission(ctx, tx, time.Now()) })
	var public *domain.Error
	if !errors.As(err, &public) || public.Code != "MONTHLY_BUDGET_EXCEEDED" {
		t.Fatal("上限未拒绝新回合", err)
	}
	// 重建服务对象与再次结算不会重发通知；已接受的运行仍可结算。
	if _, err = usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM monthly_budget_alerts").Scan(&alerts); err != nil || alerts != 2 {
		t.Fatal(alerts, err)
	}
	var patch service.SettingsPatch
	if err = json.Unmarshal([]byte(`{"monthly_budget":{"action":"warn","revision":1}}`), &patch); err != nil {
		t.Fatal(err)
	}
	if _, err = service.NewSettings(db).Update(ctx, patch, "user"); err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, db, func(tx *sql.Tx) error { return usage.CheckMonthlyAdmission(ctx, tx, time.Now()) }); err != nil {
		t.Fatal("告警模式阻止新回合", err)
	}
}

func TestMonthlyUTCDateBoundaryAndReminderRollback(t *testing.T) {
	db, runs, model := chainFixture(t)
	ctx := context.Background()
	saveBudget(t, db, 10, "stop")
	if _, err := db.Exec(`CREATE TRIGGER reject_budget_notice BEFORE INSERT ON events WHEN NEW.kind='system.notice' BEGIN SELECT RAISE(ABORT,'notice unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.Record(ctx, db, runs[2], model, provider.Usage{InputTokens: 8}); err == nil {
		t.Fatal("提醒失败仍确认结算")
	}
	for _, query := range []string{"SELECT COUNT(*) FROM usage_events", "SELECT COUNT(*) FROM monthly_budget_alerts", "SELECT SUM(cost_micro) FROM runs"} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatal("费用事务未回滚", count, err)
		}
	}
	if _, err := db.Exec("DROP TRIGGER reject_budget_notice"); err != nil {
		t.Fatal(err)
	}
	// 东八区三月一日凌晨仍属于 UTC 二月；左闭右开的自然月不能重计边界。
	clock := time.Date(2028, 3, 1, 7, 59, 59, 0, time.FixedZone("test", 8*3600))
	from, to := usage.MonthBounds(clock)
	if from.Format(time.RFC3339) != "2028-02-01T00:00:00Z" || to.Format(time.RFC3339) != "2028-03-01T00:00:00Z" {
		t.Fatal(from, to)
	}
	for i, at := range []time.Time{from.Add(-time.Millisecond), from, to.Add(-time.Millisecond), to} {
		if _, err := db.Exec("INSERT INTO usage_events(id,model_id,cost_micro,created_at) VALUES(?,?,5,?)", string(rune('a'+i)), model.ID, at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	monthly, err := usage.Monthly(ctx, db, clock)
	if err != nil || monthly.CostMicro != 10 || monthly.Calls != 2 || monthly.Status != "limit" {
		t.Fatal(monthly, err)
	}
	if err = store.Transact(ctx, db, func(tx *sql.Tx) error { return usage.CheckMonthlyAdmission(ctx, tx, to) }); err != nil {
		t.Fatal("新月份未恢复准入", err)
	}
	if err = store.Transact(ctx, db, func(tx *sql.Tx) error { return usage.NotifyMonthly(ctx, tx, clock) }); err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, db, func(tx *sql.Tx) error { return usage.NotifyMonthly(ctx, tx, to.AddDate(0, 1, 0)) }); err != nil {
		t.Fatal(err)
	}
}
