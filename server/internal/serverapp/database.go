package serverapp

import (
	"context"
	"errors"
	"strings"

	"io.yueyang/tom/internal/store/postgres"
)

// readConnectionFile 只读取受保护的连接文件；返回连接串供建连使用，错误不包含文件内容。
func readConnectionFile(path string) (string, error) {
	encoded, err := readFile(path, 16<<10, true)
	if err != nil {
		return "", err
	}
	defer clear(encoded)
	connection := strings.TrimSpace(string(encoded))
	if connection == "" || strings.ContainsRune(connection, 0) {
		return "", errors.New("PostgreSQL 连接文件无效")
	}
	return connection, nil
}

// connections 让 API 与节点登记共用相同数据库归属核验，连接原文不进入日志。
func (options DatabaseConfig) connections(roles []postgres.Role) ([]string, error) {
	connections := make([]string, len(roles))
	for index, role := range roles {
		connection, err := readConnectionFile(options.DSNFiles[role])
		if err != nil {
			return nil, err
		}
		connections[index] = connection
	}
	if err := postgres.ValidateSingleDatabase(connections); err != nil {
		return nil, err
	}
	return connections, nil
}

// initialize 在业务连接打开前迁移同一个数据库；迁移连接由执行器关闭，失败阻止启动。
// connections 为已经校验的四种业务连接，迁移凭据不能指向另一个库。
func (options DatabaseConfig) initialize(ctx context.Context, connections []string) error {
	connection, err := readConnectionFile(options.MigrationDSNFile)
	if err != nil {
		return err
	}
	if err := postgres.ValidateSingleDatabase(append([]string{connection}, connections...)); err != nil {
		return err
	}
	return postgres.Initialize(ctx, connection)
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
