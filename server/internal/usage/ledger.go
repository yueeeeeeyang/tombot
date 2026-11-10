package usage

import (
	"context"
	"database/sql"
	"math"
	"math/big"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/store"
)

// Cost 按一次调用的总额四舍五入；大整数中间量避免 token 与单价相乘溢出。
func Cost(model domain.Model, usage provider.Usage) (int64, bool, error) {
	cachePrice := model.InputPriceMicro
	estimated := usage.Estimated || model.IsPriceEstimated
	if model.CacheReadPriceMicro != nil {
		cachePrice = *model.CacheReadPriceMicro
	} else if usage.CacheReadTokens > 0 {
		estimated = true
	}
	total := new(big.Int)
	for _, pair := range [][2]int64{{int64(usage.InputTokens), model.InputPriceMicro}, {int64(usage.OutputTokens), model.OutputPriceMicro}, {int64(usage.CacheReadTokens), cachePrice}} {
		if pair[0] < 0 || pair[1] < 0 {
			return 0, false, domain.NewError(502, "PROVIDER_INVALID_USAGE", "Usage and prices must not be negative.")
		}
		total.Add(total, new(big.Int).Mul(big.NewInt(pair[0]), big.NewInt(pair[1])))
	}
	total.Add(total, big.NewInt(500_000))
	total.Quo(total, big.NewInt(1_000_000))
	if !total.IsInt64() {
		return 0, false, domain.NewError(502, "PROVIDER_INVALID_USAGE", "The reported usage exceeds supported limits.")
	}
	return total.Int64(), estimated, nil
}

// Record 原子结算执行、摘要和审查调用，返回包含并发结算的运行总费用。
func Record(ctx context.Context, db *sql.DB, run domain.Run, model domain.Model, usage provider.Usage) (int64, error) {
	cost, estimated, err := Cost(model, usage)
	if err != nil {
		return 0, err
	}
	var total int64
	recordedAt := time.Now().UTC()
	err = store.Transact(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_events(id,run_id,turn_id,bot_id,model_id,input_tokens,output_tokens,cache_read_tokens,cost_micro,is_estimated,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ids.New(), run.ID, run.TurnID, run.BotID, model.ID, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, cost, estimated, recordedAt.UnixMilli()); err != nil {
			return err
		}
		chain, err := ancestors(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		turns := map[string]bool{}
		targets := []struct{ statement, id string }{}
		for _, ancestor := range chain {
			targets = append(targets, struct{ statement, id string }{"UPDATE runs SET cost_micro=cost_micro+? WHERE id=? AND cost_micro<=?", ancestor.id})
			if !turns[ancestor.turnID] {
				turns[ancestor.turnID] = true
				targets = append(targets, struct{ statement, id string }{"UPDATE turns SET cost_micro=cost_micro+? WHERE id=? AND cost_micro<=?", ancestor.turnID})
			}
		}
		for _, target := range targets {
			result, err := tx.ExecContext(ctx, target.statement, cost, target.id, math.MaxInt64-cost)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return domain.NewError(502, "PROVIDER_INVALID_USAGE", "The cumulative usage exceeds supported limits.")
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT cost_micro FROM runs WHERE id=?", run.ID).Scan(&total); err != nil {
			return err
		}
		return NotifyMonthly(ctx, tx, recordedAt)
	})
	return total, err
}

// CheckBudget 以数据库为准，避免审查与摘要结算后仍按旧的内存余额继续操作。
func CheckBudget(ctx context.Context, db *sql.DB, runID string) error {
	chain, err := ancestors(ctx, db, runID)
	if err != nil {
		return err
	}
	for _, item := range chain {
		if item.turnBudget.Valid && item.turnCost >= item.turnBudget.Int64 {
			return domain.NewError(400, "TURN_BUDGET_EXCEEDED", "The turn or an ancestor turn reached its cost limit.")
		}
		if item.budget.Valid && item.cost >= item.budget.Int64 {
			return domain.NewError(400, "RUN_BUDGET_EXCEEDED", "The run or an ancestor reached its cost limit.")
		}
	}
	return nil
}
