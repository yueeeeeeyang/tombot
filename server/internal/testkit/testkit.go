package testkit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	secure "io.yueyang/tom/internal/infra/crypto"
	"io.yueyang/tom/internal/store"
)

// Database 为每个测试提供独立真实 SQLite，并检查资源释放错误。
func Database(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tom.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// Random 避免把看起来真实的固定凭据写入测试源码。
func Random(t *testing.T) string {
	t.Helper()
	value, err := secure.Token()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
