package usage

import (
	"context"
	"database/sql"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
)

// RecordProbe 只校验计价。用量事件表已删除，连通性测试不再落账。
func RecordProbe(_ context.Context, _ *sql.DB, model domain.Model, tokens provider.Usage, _, _ string, _ int64) error {
	_, _, err := Cost(model, tokens)
	return err
}
