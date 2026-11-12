package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/oklog/ulid/v2"
)

// Role 限定连接用途，业务请求不能使用迁移连接或组合权限组。
type Role string

const (
	TenantRole   Role = "tombot_tenant"
	IdentityRole Role = "tombot_identity"
	ControlRole  Role = "tombot_control"
	DispatchRole Role = "tombot_dispatch"
)

// Options 描述外部连接与资源限额；连接串只在建连时使用，不参与日志序列化。
type Options struct {
	DSN                string
	Role               Role
	MaxConnections     int
	ConnectTimeout     time.Duration
	StatementTimeout   time.Duration
	LockTimeout        time.Duration
	TransactionTimeout time.Duration
	ConnectionLifetime time.Duration
}

// Database 封装用途固定的连接池，租户访问必须经过事务局部身份入口。
type Database struct {
	pool    *sql.DB
	role    Role
	timeout time.Duration
}

// Role 返回固定连接用途，领域服务据此拒绝错误的数据库权限组。
func (database *Database) Role() Role { return database.role }

// Open 建立有界连接池并验证最小权限；无效配置或危险数据库角色会返回错误。
func Open(ctx context.Context, options Options) (*Database, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	configuration, err := parseConnection(options.DSN)
	if err != nil {
		return nil, err
	}
	configuration.ConnectTimeout = options.ConnectTimeout
	configuration.RuntimeParams["application_name"] = string(options.Role)
	configuration.RuntimeParams["search_path"] = "pg_catalog,tombot,pg_temp"
	configuration.RuntimeParams["timezone"] = "UTC"
	configuration.RuntimeParams["tombot.user_id"] = ""
	configuration.RuntimeParams["statement_timeout"] = milliseconds(options.StatementTimeout)
	configuration.RuntimeParams["lock_timeout"] = milliseconds(options.LockTimeout)
	configuration.RuntimeParams["idle_in_transaction_session_timeout"] = milliseconds(options.TransactionTimeout)
	pool := stdlib.OpenDB(*configuration)
	pool.SetMaxOpenConns(options.MaxConnections)
	pool.SetMaxIdleConns(options.MaxConnections)
	pool.SetConnMaxLifetime(options.ConnectionLifetime)
	connectionContext, cancel := context.WithTimeout(ctx, options.ConnectTimeout)
	defer cancel()
	if err = pool.PingContext(connectionContext); err != nil {
		return nil, errors.Join(safeDatabaseError("连接 PostgreSQL 失败", err), pool.Close())
	}
	if err = validateRole(connectionContext, pool, options.Role); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return &Database{pool: pool, role: options.Role, timeout: options.TransactionTimeout}, nil
}

func (options Options) validate() error {
	if options.DSN == "" || options.MaxConnections < 1 || options.MaxConnections > 256 {
		return errors.New("PostgreSQL 连接配置或连接池上限无效")
	}
	if options.Role != TenantRole && options.Role != IdentityRole && options.Role != ControlRole && options.Role != DispatchRole {
		return errors.New("PostgreSQL 连接用途无效")
	}
	for _, duration := range []time.Duration{options.ConnectTimeout, options.StatementTimeout, options.LockTimeout, options.TransactionTimeout, options.ConnectionLifetime} {
		if duration < time.Millisecond || duration > 24*time.Hour {
			return errors.New("PostgreSQL 超时必须介于一毫秒与一天之间")
		}
	}
	if options.LockTimeout > options.StatementTimeout || options.StatementTimeout > options.TransactionTimeout {
		return errors.New("数据库锁等待、语句与事务超时顺序无效")
	}
	return nil
}

func isLocalHost(host string) bool {
	return strings.HasPrefix(host, "/") || host == "localhost" || net.ParseIP(host).IsLoopback()
}

func parseConnection(dsn string) (*pgx.ConnConfig, error) {
	if dsn == "" {
		return nil, errors.New("缺少 PostgreSQL 连接配置")
	}
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("PostgreSQL 连接配置无效")
	}
	if configuration.TLSConfig == nil && !isLocalHost(configuration.Host) {
		return nil, errors.New("远程 PostgreSQL 连接必须启用 TLS")
	}
	for _, fallback := range configuration.Fallbacks {
		if fallback.TLSConfig == nil && !isLocalHost(fallback.Host) {
			return nil, errors.New("远程 PostgreSQL 连接禁止降级为明文")
		}
	}
	return configuration, nil
}

func milliseconds(duration time.Duration) string {
	return strconv.FormatInt(duration.Milliseconds(), 10)
}

func validateRole(ctx context.Context, pool *sql.DB, role Role) error {
	var dangerous, member, mixed bool
	err := pool.QueryRowContext(ctx, `SELECT
	    current_user <> session_user
	    OR EXISTS (SELECT 1 FROM pg_roles WHERE
	        (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication)
	        AND pg_has_role(session_user, oid, 'MEMBER'))
	    OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'tombot' AND pg_has_role(session_user, nspowner, 'MEMBER')),
	    pg_has_role(session_user, $1, 'MEMBER'),
	    EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN
	        ('tombot_tenant','tombot_identity','tombot_control','tombot_dispatch')
	        AND rolname <> $1 AND pg_has_role(session_user, oid, 'MEMBER'))`, string(role)).Scan(&dangerous, &member, &mixed)
	if err != nil {
		return safeDatabaseError("校验数据库角色失败", err)
	}
	if dangerous || !member || mixed {
		return errors.New("数据库运行角色权限过大、用途不符或混用了权限组")
	}
	return nil
}

// Tenant 在指定用户的短事务中执行业务；缺少身份时拒绝，回滚后不遗留连接上下文。
func (database *Database) Tenant(ctx context.Context, userID string, apply func(*sql.Tx) error) error {
	if database.role != TenantRole {
		return errors.New("当前数据库连接不能执行租户事务")
	}
	if _, err := ulid.ParseStrict(userID); err != nil {
		return errors.New("租户事务缺少有效用户标识")
	}
	return database.transact(ctx, userID, nil, apply)
}

// Snapshot 在同一只读快照中读取用户状态与事件水位；参数与 Tenant 相同。
func (database *Database) Snapshot(ctx context.Context, userID string, apply func(*sql.Tx) error) error {
	if database.role != TenantRole {
		return errors.New("当前数据库连接不能读取租户快照")
	}
	if _, err := ulid.ParseStrict(userID); err != nil {
		return errors.New("租户快照缺少有效用户标识")
	}
	return database.transact(ctx, userID, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, apply)
}

// Platform 执行身份、管理或调度事务；租户连接不可借此省略所属用户。
func (database *Database) Platform(ctx context.Context, apply func(*sql.Tx) error) error {
	if database.role == TenantRole {
		return errors.New("租户数据库连接必须指定用户")
	}
	return database.transact(ctx, "", nil, apply)
}

func (database *Database) transact(ctx context.Context, userID string, options *sql.TxOptions, apply func(*sql.Tx) error) (result error) {
	if apply == nil {
		return errors.New("数据库事务操作不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, database.timeout)
	defer cancel()
	transaction, err := database.pool.BeginTx(ctx, options)
	if err != nil {
		return safeDatabaseError("开始数据库事务失败", err)
	}
	defer func() {
		if err := transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, safeDatabaseError("回滚数据库事务失败", err))
		}
	}()
	if _, err = transaction.ExecContext(ctx, `SELECT set_config('tombot.user_id', $1, true)`, userID); err != nil {
		return safeDatabaseError("设置事务用户失败", err)
	}
	if err = apply(transaction); err != nil {
		return err
	}
	return safeDatabaseError("提交数据库事务失败", transaction.Commit())
}

// Close 释放连接池；关闭失败向调用方返回，不能假定资源已释放。
func (database *Database) Close() error { return database.pool.Close() }

func safeDatabaseError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	if errors.Is(cause, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	// 驱动错误可能包含连接串或行内容，只保留可用于定位的稳定错误分类。
	var sqlState interface{ SQLState() string }
	if errors.As(cause, &sqlState) {
		return fmt.Errorf("%s（SQLSTATE %s）", operation, sqlState.SQLState())
	}
	return errors.New(operation)
}
