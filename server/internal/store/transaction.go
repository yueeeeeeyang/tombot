package store

import (
	"context"
	"database/sql"
	"errors"

	"io.yueyang/tom/internal/domain"
	"modernc.org/sqlite"
)

// Transact 在单写事务中执行业务变更，失败必须回滚并保留回滚错误。
func Transact(ctx context.Context, db *sql.DB, apply func(*sql.Tx) error) (result error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if result != nil {
			err := tx.Rollback()
			if !errors.Is(err, sql.ErrTxDone) {
				result = errors.Join(result, err)
			}
		}
	}()
	if err = apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Audit 不再写入。审批、用量和审计已经从桌面和服务里去掉。
func Audit(context.Context, *sql.Tx, string, string, string, any) error {
	return nil
}

// ConstraintError 隐藏 SQLite 原始错误，避免响应暴露内部数据和表名。
func ConstraintError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == 19 {
		return domain.NewError(409, "RESOURCE_CONFLICT", "The resource is duplicated or still referenced.")
	}
	return err
}

// RequireChanged 将更新不存在资源映射为 404；数据库错误原样向上传递。
func RequireChanged(result sql.Result, err error) error {
	if err != nil {
		return ConstraintError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return domain.NewError(404, "NOT_FOUND", "The resource does not exist.")
	}
	return nil
}
