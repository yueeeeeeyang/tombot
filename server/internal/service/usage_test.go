package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/usage"
)

func TestUsageGroupsPaginationDeletedObjectsAndRangeValidation(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	service := NewUsage(db)
	from, to := usage.MonthBounds(time.Now())
	for i, bot := range []any{nil, "deleted-a", "deleted-b"} {
		if _, err := db.Exec(`INSERT INTO usage_events(id,bot_id,model_id,input_tokens,output_tokens,cache_read_tokens,cost_micro,is_estimated,created_at) VALUES(?,?,'deleted-model',2,3,4,5,?,?)`, string(rune('a'+i)), bot, i%2, from.Add(time.Duration(i)*24*time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	for _, groupBy := range []string{"bot", "model", "day"} {
		query := UsageQuery{From: from, To: to, GroupBy: groupBy, Limit: 1}
		seen := map[string]bool{}
		for range 5 {
			report, err := service.Report(ctx, query)
			if err != nil || report.Total.Calls != 3 || report.Total.CostMicro != 15 || report.Total.CacheReadTokens != 12 || report.Total.EstimatedCalls != 1 || report.Monthly.CostMicro != 15 {
				t.Fatal(report, err)
			}
			for _, group := range report.Groups {
				if seen[group.ID] {
					t.Fatal("分页重复", group.ID)
				}
				seen[group.ID] = true
				if groupBy == "model" && group.Name != "已删除的模型" {
					t.Fatal(group)
				}
			}
			if report.NextCursor == "" {
				break
			}
			query.Cursor = report.NextCursor
		}
		expected := 3
		if groupBy == "model" {
			expected = 1
		}
		if len(seen) != expected {
			t.Fatal("分页遗漏", groupBy, seen)
		}
	}
	for _, query := range []UsageQuery{{From: to, To: from}, {From: from}, {From: from, To: from.Add(367 * 24 * time.Hour)}, {GroupBy: "sql"}, {Limit: 201}, {Cursor: strings.Repeat("a", 129)}} {
		if _, err := service.Report(ctx, query); err == nil {
			t.Fatal("无效查询被接受", query)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.Report(canceled, UsageQuery{}); err == nil {
		t.Fatal("取消查询仍成功")
	}
}

func TestMonthlySettingsCASPartialPatchAndCorruption(t *testing.T) {
	db := testkit.Database(t)
	ctx := context.Background()
	settings := NewSettings(db)
	update := func(encoded string) error {
		var patch SettingsPatch
		if err := json.Unmarshal([]byte(encoded), &patch); err != nil {
			t.Fatal(err)
		}
		_, err := settings.Update(ctx, patch, "user")
		return err
	}
	if err := update(`{"monthly_budget":{"revision":0,"limit_micro":100,"warn_percent":75,"action":"warn"}}`); err != nil {
		t.Fatal(err)
	}
	if err := update(`{"monthly_budget":{"revision":0,"limit_micro":200}}`); err == nil {
		t.Fatal("旧版本覆盖配置")
	}
	if err := update(`{"reviewer":{"enabled":true},"monthly_budget":{"revision":1,"action":"stop"}}`); err != nil {
		t.Fatal(err)
	}
	saved, err := settings.Get(ctx)
	if err != nil || !saved.Reviewer.Enabled || *saved.MonthlyBudget.LimitMicro != 100 || saved.MonthlyBudget.WarnPercent != 75 || saved.MonthlyBudget.Revision != 2 {
		t.Fatal(saved, err)
	}
	for _, encoded := range []string{`{"monthly_budget":{}}`, `{"monthly_budget":{"revision":2}}`, `{"monthly_budget":{"revision":2,"limit_micro":0}}`, `{"monthly_budget":{"revision":2,"limit_micro":1000000000001}}`, `{"monthly_budget":{"revision":2,"action":null}}`, `{"monthly_budget":{"revision":2,"warn_percent":null}}`, `{"monthly_budget":{"revision":2,"warn_percent":100}}`, `{"monthly_budget":{"revision":2,"action":"ignore"}}`} {
		if err = update(encoded); err == nil {
			t.Fatal("无效预算被接受", encoded)
		}
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_monthly_audit BEFORE INSERT ON audit_log WHEN NEW.target='monthly_budget' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = update(`{"reviewer":{"enabled":false},"monthly_budget":{"revision":2,"limit_micro":null}}`); err == nil {
		t.Fatal("审计失败仍保存")
	}
	saved, err = settings.Get(ctx)
	if err != nil || !saved.Reviewer.Enabled || saved.MonthlyBudget.Revision != 2 {
		t.Fatal("多配置事务未回滚", saved, err)
	}
	if _, err = db.Exec("DROP TRIGGER reject_monthly_audit"); err != nil {
		t.Fatal(err)
	}
	if err = update(`{"monthly_budget":{"revision":2,"limit_micro":null}}`); err != nil {
		t.Fatal(err)
	}
	saved, err = settings.Get(ctx)
	if err != nil || saved.MonthlyBudget.LimitMicro != nil || saved.MonthlyBudget.Action != "stop" {
		t.Fatal(saved, err)
	}
	for _, encoded := range []string{"null", "{}", `{"action":"stop","warn_percent":80,"revision":1}`, `{"limit_micro":null,"action":"stop","warn_percent":80,"revision":0}`, `{"limit_micro":null,"action":"stop","warn_percent":80,"revision":1,"extra":0}`, `{"limit_micro":null,"action":"stop","warn_percent":80,"revision":1} {}`} {
		if _, err = db.Exec("UPDATE settings SET value_json=? WHERE key='monthly_budget'", encoded); err != nil {
			t.Fatal(err)
		}
		if _, err = settings.Get(ctx); err == nil {
			t.Fatal("损坏预算被放行", encoded)
		}
	}
}
