package usage

import (
	"context"
	"database/sql"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/store"
)

// RecordProbe 记录不属于 Bot 回合的真实连通性调用；计价仍使用统一规则。
func RecordProbe(ctx context.Context, db *sql.DB, model domain.Model, tokens provider.Usage, providerID, actor string, latencyMS int64) error {
	cost, estimated, err := Cost(model, tokens)
	if err != nil {
		return err
	}
	recordedAt := time.Now().UTC()
	return store.Transact(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_events(id,model_id,input_tokens,output_tokens,cache_read_tokens,cost_micro,is_estimated,created_at) VALUES(?,?,?,?,?,?,?,?)`, ids.New(), model.ID, tokens.InputTokens, tokens.OutputTokens, tokens.CacheReadTokens, cost, estimated, recordedAt.UnixMilli()); err != nil {
			return err
		}
		if err := store.Audit(ctx, tx, actor, "provider.test", providerID, map[string]any{"latency_ms": latencyMS, "model_id": model.ID, "cost_micro": cost, "is_estimated": estimated}); err != nil {
			return err
		}
		return NotifyMonthly(ctx, tx, recordedAt)
	})
}
