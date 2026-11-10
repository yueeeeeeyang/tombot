package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	safelog "io.yueyang/tom/internal/infra/log"
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

// Audit 与业务操作共用事务；详情只允许业务标识、字段名及脱敏摘要。
func Audit(ctx context.Context, tx *sql.Tx, actor, action, target string, details any) error {
	actor, action, target = safelog.SafeText(actor), safelog.SafeText(action), safelog.SafeText(target)
	if len(actor) > 256 || len(action) > 128 || len(target) > 4096 {
		return fmt.Errorf("审计标识超出容量")
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	encoded, err = safelog.SafeDetails(encoded)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_log(id,actor,action,target,detail_json,created_at) VALUES(?,?,?,?,?,?)", ids.New(), actor, action, target, string(encoded), time.Now().UnixMilli())
	return err
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
