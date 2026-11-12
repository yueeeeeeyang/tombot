package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open 创建单写连接并执行迁移；所有连接均启用外键与忙等待。
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(ON)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err == nil {
		err = Migrate(ctx, db)
	}
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", errorsWithClose(db, err))
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, errorsWithClose(db, err)
	}
	return db, nil
}

func errorsWithClose(db *sql.DB, cause error) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf("%w；关闭数据库失败: %v", cause, err)
	}
	return cause
}
