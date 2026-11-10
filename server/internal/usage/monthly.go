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

// Totals 不再读取用量事件。账本已删除，汇总保持为零。
func Totals(context.Context, Querier, time.Time, time.Time) (domain.UsageTotals, error) {
	return domain.UsageTotals{}, nil
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

// CheckMonthlyAdmission 不再按月用量拦截新回合。
func CheckMonthlyAdmission(context.Context, *sql.Tx, time.Time) error {
	return nil
}

// NotifyMonthly 不再写用量提醒。
func NotifyMonthly(context.Context, *sql.Tx, time.Time) error {
	return nil
}
