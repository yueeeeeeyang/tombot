package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int64
	name     string
	checksum string
	script   string
}

// Migrate 在受限时间内校验并执行新 PostgreSQL 链；不识别的非空库和历史篡改直接失败。
func Migrate(ctx context.Context, pool *sql.DB) error {
	return migrate(ctx, pool, migrationFiles)
}

func migrate(ctx context.Context, pool *sql.DB, source fs.FS) (result error) {
	if pool == nil {
		return errors.New("迁移数据库连接不能为空")
	}
	migrations, err := readMigrations(source)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	transaction, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return safeDatabaseError("开始数据库初始化失败", err)
	}
	defer func() {
		if err := transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			result = errors.Join(result, safeDatabaseError("回滚数据库初始化失败", err))
		}
	}()
	if _, err = transaction.ExecContext(ctx, `SET LOCAL search_path = pg_catalog, tombot, pg_temp;
	    SET LOCAL lock_timeout = '10s'; SET LOCAL statement_timeout = '45s';
	    SELECT pg_advisory_xact_lock(hashtextextended('tombot.schema.migrations', 0));`); err != nil {
		return safeDatabaseError("取得数据库初始化锁失败", err)
	}
	count, err := verifyHistory(ctx, transaction, migrations)
	if err != nil {
		return err
	}
	for _, next := range migrations[count:] {
		if _, err = transaction.ExecContext(ctx, next.script); err != nil {
			return safeDatabaseError("执行数据库脚本 "+next.name+" 失败", err)
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO tombot.schema_migrations(version, name, checksum) VALUES ($1,$2,$3)`, next.version, next.name, next.checksum); err != nil {
			return safeDatabaseError("写入数据库版本账本失败", err)
		}
	}
	return safeDatabaseError("提交数据库初始化失败", transaction.Commit())
}

func readMigrations(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取数据库脚本目录失败: %w", err)
	}
	namePattern := regexp.MustCompile(`^[0-9]{4,}_[a-z][a-z0-9_]*\.sql$`)
	var migrations []migration
	var previous int64
	for _, entry := range entries {
		if entry.IsDir() || !namePattern.MatchString(entry.Name()) {
			return nil, errors.New("数据库脚本名称必须包含唯一版本和语义描述")
		}
		version, err := strconv.ParseInt(strings.SplitN(entry.Name(), "_", 2)[0], 10, 64)
		if err != nil || version <= previous {
			return nil, errors.New("数据库脚本版本必须严格递增且不可重复")
		}
		script, err := fs.ReadFile(source, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("读取数据库脚本失败: %w", err)
		}
		if strings.TrimSpace(string(script)) == "" {
			return nil, errors.New("数据库脚本不能为空")
		}
		migrations = append(migrations, migration{version, entry.Name(), fmt.Sprintf("%x", sha256.Sum256(script)), string(script)})
		previous = version
	}
	if len(migrations) == 0 {
		return nil, errors.New("缺少 PostgreSQL 基线脚本")
	}
	return migrations, nil
}

func verifyHistory(ctx context.Context, transaction *sql.Tx, migrations []migration) (count int, result error) {
	var schemaExists, historyExists bool
	if err := transaction.QueryRowContext(ctx, `SELECT
	    EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'tombot'),
	    to_regclass('tombot.schema_migrations') IS NOT NULL`).Scan(&schemaExists, &historyExists); err != nil {
		return 0, safeDatabaseError("读取数据库版本状态失败", err)
	}
	if !schemaExists {
		var hasObjects bool
		if err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
	        SELECT 1 FROM pg_class AS relation JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
	        WHERE namespace.nspname NOT IN ('pg_catalog', 'information_schema')
	          AND namespace.nspname NOT LIKE 'pg_toast%' AND namespace.nspname NOT LIKE 'pg_temp%'
	          AND relation.relkind IN ('r','p','v','m','S','f'))`).Scan(&hasObjects); err != nil {
			return 0, safeDatabaseError("检查数据库是否为空失败", err)
		}
		if hasObjects {
			return 0, errors.New("首次初始化必须使用空 PostgreSQL 数据库，禁止转换或覆盖现有数据")
		}
		return 0, nil
	}
	if !historyExists {
		return 0, errors.New("现有数据库缺少 PostgreSQL 版本账本，禁止覆盖初始化")
	}
	rows, err := transaction.QueryContext(ctx, `SELECT version, name, checksum FROM tombot.schema_migrations ORDER BY version`)
	if err != nil {
		return 0, safeDatabaseError("读取数据库版本账本失败", err)
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	for rows.Next() {
		if count >= len(migrations) {
			return 0, errors.New("数据库版本高于程序版本，禁止降级启动")
		}
		var version int64
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			return 0, safeDatabaseError("读取数据库版本记录失败", err)
		}
		expected := migrations[count]
		if version != expected.version || name != expected.name || checksum != expected.checksum {
			return 0, errors.New("PostgreSQL 历史迁移校验失败，禁止修改历史或跳过版本")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return 0, safeDatabaseError("遍历数据库版本账本失败", err)
	}
	if count == 0 {
		return 0, errors.New("现有数据库版本账本为空，禁止假定初始化已完成")
	}
	return count, nil
}
