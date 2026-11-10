package usage

import (
	"context"
	"database/sql"
	"io.yueyang/tom/internal/domain"
)

type budgetQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type runBudget struct {
	id, turnID         string
	parentID           sql.NullString
	cost, turnCost     int64
	budget, turnBudget sql.NullInt64
}

// ancestors 最多读取四跳父链并检测环与断链；费用或预算不能因损坏的父链被跳过。
func ancestors(ctx context.Context, query budgetQuery, runID string) ([]runBudget, error) {
	chain := []runBudget{}
	seen := map[string]bool{}
	for runID != "" {
		if seen[runID] || len(chain) > domain.MaxMessageHops {
			return nil, domain.NewError(500, "RUN_ANCESTRY_INVALID", "The run ancestry is cyclic or exceeds its depth limit.")
		}
		seen[runID] = true
		var item runBudget
		if err := query.QueryRowContext(ctx, `SELECT r.id,r.turn_id,r.parent_run_id,r.cost_micro,r.budget_micro,t.cost_micro,t.budget_micro FROM runs r JOIN turns t ON t.id=r.turn_id WHERE r.id=?`, runID).Scan(&item.id, &item.turnID, &item.parentID, &item.cost, &item.budget, &item.turnCost, &item.turnBudget); err != nil {
			return nil, err
		}
		chain = append(chain, item)
		runID = item.parentID.String
	}
	return chain, nil
}

// RemainingBudget 在调用方事务中计算所有祖先和回合的最小剩余额度，用于创建子执行。
func RemainingBudget(ctx context.Context, tx *sql.Tx, runID string, maximum int64) (int64, error) {
	chain, err := ancestors(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	remaining := maximum
	for _, item := range chain {
		if item.budget.Valid {
			remaining = min(remaining, item.budget.Int64-item.cost)
		}
		if item.turnBudget.Valid {
			remaining = min(remaining, item.turnBudget.Int64-item.turnCost)
		}
	}
	if remaining <= 0 {
		return 0, domain.NewError(400, "RUN_BUDGET_EXCEEDED", "The calling run or an ancestor reached its cost limit.")
	}
	return remaining, nil
}
