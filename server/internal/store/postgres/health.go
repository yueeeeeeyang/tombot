package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// ValidateSingleDatabase 要求各用途使用同一明确主机、端口和库名，防止将身份与业务接到不同数据库。
// 各用途可以有独立凭据与 TLS 设置；不接受会切到其他地址的多主机连接配置。
func ValidateSingleDatabase(connections []string) error {
	if len(connections) == 0 {
		return errors.New("缺少 PostgreSQL 用途连接")
	}
	var host, name string
	var port uint16
	for index, connection := range connections {
		configuration, err := parseConnection(connection)
		if err != nil {
			return err
		}
		if index == 0 {
			host, port, name = configuration.Host, configuration.Port, configuration.Database
		}
		if configuration.Host != host || configuration.Port != port || configuration.Database != name {
			return errors.New("各用途 PostgreSQL 连接必须使用同一主机、端口和数据库")
		}
		for _, fallback := range configuration.Fallbacks {
			if fallback.Host != host || fallback.Port != port {
				return errors.New("PostgreSQL 用途连接不能切换数据库地址")
			}
		}
	}
	return nil
}

// VerifyCurrent 只读核对完整迁移账本；运行服务不持有迁移权限，不自动补齐或转换数据库。
func (database *Database) VerifyCurrent(ctx context.Context) (failure error) {
	migrations, err := readMigrations(migrationFiles)
	if err != nil {
		return err
	}
	transaction, err := database.pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return safeDatabaseError("读取 PostgreSQL 运行版本失败", err)
	}
	defer func() {
		if err := transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			failure = errors.Join(failure, safeDatabaseError("结束 PostgreSQL 版本检查失败", err))
		}
	}()
	count, err := verifyHistory(ctx, transaction, migrations)
	if err != nil {
		return err
	}
	if count != len(migrations) {
		return errors.New("PostgreSQL 尚未执行全部迁移，请先运行 tomdb")
	}
	return nil
}

// Ping 只确认当前用途连接可用，不代替启动时的权限与版本校验。
func (database *Database) Ping(ctx context.Context) error {
	if err := database.pool.PingContext(ctx); err != nil {
		return safeDatabaseError("PostgreSQL 连接暂不可用", err)
	}
	return nil
}
