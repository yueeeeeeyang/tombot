package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// CheckUpgrade 只读核验候选程序与数据库账本，提前拒绝降级或历史篡改，不执行迁移。
func CheckUpgrade(ctx context.Context, dsn string) (result error) {
	configuration, err := parseConnection(dsn)
	if err != nil {
		return err
	}
	configuration.ConnectTimeout = 10 * time.Second
	pool := stdlib.OpenDB(*configuration)
	pool.SetMaxOpenConns(1)
	defer func() { result = errors.Join(result, pool.Close()) }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	migrations, err := readMigrations(migrationFiles)
	if err != nil {
		return err
	}
	transaction, err := pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return safeDatabaseError("读取升级前数据库状态失败", err)
	}
	defer func() {
		result = errors.Join(result, safeDatabaseError("关闭升级前检查事务失败", transaction.Rollback()))
	}()
	_, err = verifyHistory(ctx, transaction, migrations)
	return err
}

// Initialize 使用独立迁移连接初始化或升级数据库，结束时关闭连接；错误不外传连接配置和敏感内容。
func Initialize(ctx context.Context, dsn string) (result error) {
	configuration, err := parseConnection(dsn)
	if err != nil {
		return err
	}
	configuration.ConnectTimeout = 10 * time.Second
	configuration.RuntimeParams["application_name"] = "tombot_migration"
	configuration.RuntimeParams["search_path"] = "pg_catalog,tombot,pg_temp"
	pool := stdlib.OpenDB(*configuration)
	pool.SetMaxOpenConns(1)
	defer func() { result = errors.Join(result, pool.Close()) }()
	return Migrate(ctx, pool)
}
