package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// Initialize 使用外部注入的迁移连接初始化新库；连接配置和数据库错误中的敏感内容不外传。
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
