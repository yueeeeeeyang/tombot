package serverapp

import (
	"context"
	"errors"
	"strings"

	"io.yueyang/tom/internal/store/postgres"
)

// connections 让 API 与本机引导共用相同数据库归属核验，连接原文不进入日志。
func (options DatabaseConfig) connections(roles []postgres.Role) ([]string, error) {
	connections := make([]string, len(roles))
	for index, role := range roles {
		encoded, err := readFile(options.DSNFiles[role], 16<<10, true)
		if err != nil {
			return nil, err
		}
		connections[index] = strings.TrimSpace(string(encoded))
		clear(encoded)
		if strings.ContainsRune(connections[index], 0) {
			return nil, errors.New("PostgreSQL 连接文件无效")
		}
	}
	if err := postgres.ValidateSingleDatabase(connections); err != nil {
		return nil, err
	}
	return connections, nil
}

// open 固定用途并核验完整迁移账本，失败立即释放连接，部署命令不能绕过版本检查。
func (options DatabaseConfig) open(ctx context.Context, role postgres.Role, dsn string) (*postgres.Database, error) {
	database, err := postgres.Open(ctx, postgres.Options{DSN: dsn, Role: role, MaxConnections: options.MaxConnections, ConnectTimeout: options.ConnectTimeout.Value(), StatementTimeout: options.StatementTimeout.Value(), LockTimeout: options.LockTimeout.Value(), TransactionTimeout: options.TransactionTimeout.Value(), ConnectionLifetime: options.ConnectionLifetime.Value()})
	if err != nil {
		return nil, err
	}
	check, cancel := context.WithTimeout(ctx, options.TransactionTimeout.Value())
	defer cancel()
	if err = database.VerifyCurrent(check); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return database, nil
}
