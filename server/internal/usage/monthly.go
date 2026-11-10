package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/store"
)

type Querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ReadMonthlyBudget 严格读取预算；损坏配置必须显式失败，不能放行新任务。
func ReadMonthlyBudget(ctx context.Context, q Querier) (domain.MonthlyBudget, error) {
	budget := domain.MonthlyBudget{WarnPercent: 80, Action: "stop"}
	var encoded string
	err := q.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key='monthly_budget'").Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return budget, nil
	}
	if err != nil {
		return budget, err
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var stored *domain.MonthlyBudget
	if err = decoder.Decode(&stored); err != nil {
		return budget, err
	}
	if stored == nil {
		return budget, errors.New("月度预算配置为空")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(encoded), &fields); err != nil {
		return budget, err
	}
	if _, present := fields["limit_micro"]; !present {
		return budget, errors.New("月度预算缺少额度字段")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return budget, errors.New("月度预算配置含尾随内容")
	}
	if err = ValidateMonthlyBudget(*stored); err != nil {
		return budget, err
	}
	if stored.Revision < 1 {
		return budget, errors.New("月度预算版本无效")
	}
	return *stored, nil
}

func ValidateMonthlyBudget(budget domain.MonthlyBudget) error {
	if budget.LimitMicro != nil && (*budget.LimitMicro < 1 || *budget.LimitMicro > 1_000_000_000_000) || budget.WarnPercent < 1 || budget.WarnPercent > 99 || (budget.Action != "stop" && budget.Action != "warn") {
		return domain.NewError(400, "MONTHLY_BUDGET_INVALID", "Use a positive limit up to 1000000000000 micro USD, a warning percentage from 1 to 99, and stop or warn.")
	}
	return nil
}

// MonthBounds 固定 UTC，避免主机时区、夏令时及客户端切换时区改变已结算月份。
func MonthBounds(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

const totalsSQL = `COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cost_micro),0),COALESCE(SUM(CASE WHEN is_estimated!=0 THEN 1 ELSE 0 END),0)`

func Totals(ctx context.Context, q Querier, from, to time.Time) (domain.UsageTotals, error) {
	var totals domain.UsageTotals
	err := q.QueryRowContext(ctx, "SELECT "+totalsSQL+" FROM usage_events WHERE created_at>=? AND created_at<?", from.UnixMilli(), to.UnixMilli()).Scan(&totals.Calls, &totals.InputTokens, &totals.OutputTokens, &totals.CacheReadTokens, &totals.CostMicro, &totals.EstimatedCalls)
	return totals, err
}

func Monthly(ctx context.Context, q Querier, now time.Time) (domain.MonthlyUsage, error) {
	var monthly domain.MonthlyUsage
	budget, err := ReadMonthlyBudget(ctx, q)
	if err != nil {
		return monthly, err
	}
	from, to := MonthBounds(now)
	totals, err := Totals(ctx, q, from, to)
	monthly = domain.MonthlyUsage{Month: from.Format("2006-01"), UsageTotals: totals, Budget: budget, Status: "unlimited"}
	if budget.LimitMicro != nil {
		monthly.Status = "normal"
		if totals.CostMicro >= (*budget.LimitMicro*int64(budget.WarnPercent)+99)/100 {
			monthly.Status = "warning"
		}
		if totals.CostMicro >= *budget.LimitMicro {
			monthly.Status = "limit"
		}
	}
	return monthly, err
}

// CheckMonthlyAdmission 必须在创建回合的同一写事务内执行；不预占未产生的费用。
func CheckMonthlyAdmission(ctx context.Context, tx *sql.Tx, now time.Time) error {
	budget, err := ReadMonthlyBudget(ctx, tx)
	if err != nil {
		return err
	}
	if budget.LimitMicro == nil || budget.Action != "stop" {
		return nil
	}
	from, to := MonthBounds(now)
	totals, err := Totals(ctx, tx, from, to)
	if err != nil {
		return err
	}
	if totals.CostMicro >= *budget.LimitMicro {
		return domain.NewError(409, "MONTHLY_BUDGET_EXCEEDED", "The UTC monthly budget has been reached. New turns are paused until next month or a budget change.")
	}
	return nil
}

// NotifyMonthly 在费用或配置事务内去重并持久化通知，失败时整体回滚。
func NotifyMonthly(ctx context.Context, tx *sql.Tx, now time.Time) error {
	budget, err := ReadMonthlyBudget(ctx, tx)
	if err != nil || budget.LimitMicro == nil {
		return err
	}
	monthly, err := Monthly(ctx, tx, now)
	if err != nil {
		return err
	}
	levels := []string{}
	if monthly.Status == "warning" || monthly.Status == "limit" {
		levels = append(levels, "warning")
	}
	if monthly.Status == "limit" {
		levels = append(levels, "limit")
	}
	for _, level := range levels {
		result, err := tx.ExecContext(ctx, "INSERT INTO monthly_budget_alerts(month,level,created_at) VALUES(?,?,?) ON CONFLICT(month,level) DO NOTHING", monthly.Month, level, now.UnixMilli())
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			continue
		}
		text := "本月模型费用已达到提醒阈值，请查看用量与预算。"
		if level == "limit" {
			text = "本月模型费用已达到预算上限，当前配置仅告警。"
			if budget.Action == "stop" {
				text = "本月模型费用已达到预算上限，已暂停接受新任务；已接受的任务继续执行。"
			}
		}
		payload := map[string]any{"level": "warning", "text": text, "code": "MONTHLY_BUDGET", "month": monthly.Month, "stage": level, "cost_micro": monthly.CostMicro, "limit_micro": budget.LimitMicro}
		if _, err = bus.Append(ctx, tx, "system.notice", "", payload); err != nil {
			return err
		}
		if err = store.Audit(ctx, tx, "system", "usage.budget_alert", monthly.Month, payload); err != nil {
			return err
		}
	}
	return nil
}
