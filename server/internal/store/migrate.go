package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

type migration struct {
	version        int
	name, checksum string
	script         []byte
}

func loadMigration(file fs.DirEntry) (migration, error) {
	name := file.Name()
	version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
	if err != nil {
		return migration{}, err
	}
	script, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		return migration{}, err
	}
	return migration{version, name, fmt.Sprintf("%x", sha256.Sum256(script)), script}, nil
}

type migrationReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// checkedHistory 要求账本是内嵌脚本的连续前缀，返回已验证数量。备份校验只读，启动时在迁移事务内复用。
func checkedHistory(ctx context.Context, reader migrationReader, files []fs.DirEntry) (count int, result error) {
	rows, err := reader.QueryContext(ctx, "SELECT version,name,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, err
	}
	defer func() { result = errors.Join(result, rows.Close()) }()
	for rows.Next() {
		if count >= len(files) {
			return 0, fmt.Errorf("数据库迁移版本高于当前程序，禁止降级启动")
		}
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			return 0, err
		}
		expected, err := loadMigration(files[count])
		if err != nil {
			return 0, err
		}
		if version != expected.version || name != expected.name || checksum != expected.checksum {
			return 0, fmt.Errorf("历史迁移 %s 校验失败", expected.name)
		}
		count++
	}
	return count, rows.Err()
}

// VerifyHistory 验证已初始化快照的迁移链完整、版本可读，全程不修改数据库。
func VerifyHistory(ctx context.Context, db *sql.DB) error {
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	count, err := checkedHistory(ctx, db, files)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("数据库缺少已应用的迁移")
	}
	return nil
}

// Migrate 在一个事务中校验历史迁移并前向执行新版本；历史内容变动即失败。
func Migrate(ctx context.Context, db *sql.DB) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				err = fmt.Errorf("%w；回滚失败: %v", err, rollbackErr)
			}
		}
	}()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at INTEGER NOT NULL)`)
	if err != nil {
		return err
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	count, err := checkedHistory(ctx, tx, files)
	if err != nil {
		return err
	}
	for _, file := range files[count:] {
		next, err := loadMigration(file)
		if err != nil {
			return err
		}
		// 迁移账本由迁移器先创建，初始 DDL 的同名声明只在执行时变成幂等声明。
		statement := strings.Replace(string(next.script), "CREATE TABLE schema_migrations (", "CREATE TABLE IF NOT EXISTS schema_migrations (", 1)
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("迁移 %s 失败: %w", file.Name(), err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?,?)", next.version, next.name, next.checksum, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
