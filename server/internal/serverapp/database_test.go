package serverapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/store/postgres"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMigrationConnectionValidation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "socket")
	connection := "host=" + socket + " port=5432 dbname=application"
	for _, fixture := range []struct {
		name, connection, reason string
		mode                     os.FileMode
	}{
		{"空文件", "", "权限或大小无效", 0600},
		{"只有空白", " \n", "连接文件无效", 0600},
		{"包含空字节", connection + "\x00", "连接文件无效", 0600},
		{"不同数据库", "host=" + socket + " port=5432 dbname=other", "同一主机、端口和数据库", 0600},
		{"不同主机", "host=" + socket + "-other port=5432 dbname=application", "同一主机、端口和数据库", 0600},
		{"不同端口", "host=" + socket + " port=5433 dbname=application", "同一主机、端口和数据库", 0600},
		{"不同候选地址", "host=" + socket + "," + socket + "-other port=5432 dbname=application", "不能切换数据库地址", 0600},
		{"明文远程连接", "host=database.example.test dbname=application sslmode=disable", "必须启用 TLS", 0600},
		{"公开连接文件", connection, "权限或大小无效", 0644},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "migration.connection")
			writeTestFile(t, path, []byte(fixture.connection))
			if err := os.Chmod(path, fixture.mode); err != nil {
				t.Fatal(err)
			}
			configuration := DatabaseConfig{MigrationDSNFile: path}
			if err := configuration.initialize(t.Context(), []string{connection}); err == nil || !strings.Contains(err.Error(), fixture.reason) {
				t.Fatal("迁移连接未按预期在访问数据库之前拒绝", err)
			}
		})
	}
}

// migrationConnectionsClosed 检查迁移连接已释放，不把启动专用权限带入服务连接池。
func migrationConnectionsClosed(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name='tombot_migration'`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("启动迁移连接未释放")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartupMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	configuration, _, pool, name := prepareApplication(t, cluster)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.Exec(statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	openAndClose := func(ctx context.Context, candidate Config) error {
		application, err := Open(ctx, candidate, testAdminPage(t), testLogger())
		if err != nil {
			return err
		}
		return application.Close()
	}

	t.Run("空库并发启动与重复启动", func(t *testing.T) {
		// 只清空本测试的独立数据库；保留集群登录角色，复现部署已准备身份但尚未建表的状态。
		execute(`DROP SCHEMA tombot CASCADE`)
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- openAndClose(t.Context(), configuration) }()
		}
		for range 2 {
			if err := <-results; err != nil {
				t.Error("空库启动自动迁移失败", err)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
		var before, after string
		const history = `SELECT string_agg(version::text || ':' || checksum || ':' || applied_at::text, ',' ORDER BY version) FROM tombot.schema_migrations`
		if err := pool.QueryRow(history).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := openAndClose(t.Context(), configuration); err != nil {
			t.Fatal("重复启动失败", err)
		}
		if err := pool.QueryRow(history).Scan(&after); err != nil || before != after {
			t.Fatal("重复启动重写了已执行迁移", err)
		}
		migrationConnectionsClosed(t, pool, name)
	})

	t.Run("启动补齐待执行迁移", func(t *testing.T) {
		// 使用仓库的原始历史脚本准备前一版本，确保启动实际执行新 SQL，而非仅补一条账本。
		execute(`DROP SCHEMA tombot CASCADE`)
		directory := "../store/postgres/migrations"
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) < 2 {
			t.Fatal("无法准备历史数据库", err)
		}
		for _, entry := range entries[:len(entries)-1] {
			script, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			execute(string(script))
			execute(`INSERT INTO tombot.schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, strings.SplitN(entry.Name(), "_", 2)[0], entry.Name(), fmt.Sprintf("%x", sha256.Sum256(script)))
		}
		if err := openAndClose(t.Context(), configuration); err != nil {
			t.Fatal("启动未补齐迁移", err)
		}
		var restored int
		if err := pool.QueryRow(`SELECT count(*) FROM tombot.schema_migrations`).Scan(&restored); err != nil || restored != len(entries) {
			t.Fatal("未执行完整迁移链", err)
		}
		var name string
		if err := pool.QueryRow(`SELECT name FROM tombot.schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&name); err != nil || name != entries[len(entries)-1].Name() {
			t.Fatal(err)
		}
	})

	t.Run("篡改和高版本阻止启动", func(t *testing.T) {
		var checksum string
		if err := pool.QueryRow(`SELECT checksum FROM tombot.schema_migrations WHERE version=29`).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		execute(`UPDATE tombot.schema_migrations SET checksum=repeat('0',64) WHERE version=29`)
		if err := openAndClose(t.Context(), configuration); err == nil || !strings.Contains(err.Error(), "历史迁移") {
			t.Error("篡改账本未阻止启动", err)
		}
		execute(`UPDATE tombot.schema_migrations SET checksum=$1 WHERE version=29`, checksum)
		execute(`INSERT INTO tombot.schema_migrations(version,name,checksum) VALUES(9999,'9999_future.sql',repeat('0',64))`)
		if err := openAndClose(t.Context(), configuration); err == nil || !strings.Contains(err.Error(), "高于程序") {
			t.Error("高版本账本未阻止启动", err)
		}
		execute(`DELETE FROM tombot.schema_migrations WHERE version=9999`)
		migrationConnectionsClosed(t, pool, name)
	})

	t.Run("取消和锁等待超时阻止启动", func(t *testing.T) {
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := openAndClose(canceled, configuration); err == nil {
			t.Fatal("取消后仍允许启动")
		}
		transaction, err := pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := transaction.Rollback(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := transaction.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('tombot.schema.migrations', 0))`); err != nil {
			t.Fatal(err)
		}
		bounded, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer stop()
		if err := openAndClose(bounded, configuration); err == nil {
			t.Fatal("锁等待超时后仍允许启动")
		}
		migrationConnectionsClosed(t, pool, name)
	})

	t.Run("迁移权限不足不遗留半成品", func(t *testing.T) {
		execute(`DROP SCHEMA tombot CASCADE`)
		candidate := configuration
		candidate.Database.MigrationDSNFile = configuration.Database.DSNFiles[postgres.ControlRole]
		if err := openAndClose(t.Context(), candidate); err == nil {
			t.Fatal("业务身份被用于创建数据库结构")
		}
		var exists bool
		if err := pool.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='tombot')`).Scan(&exists); err != nil || exists {
			t.Fatal("迁移失败遗留数据库结构", err)
		}
		migrationConnectionsClosed(t, pool, name)
		if err := openAndClose(t.Context(), configuration); err != nil {
			t.Fatal("修正迁移身份后不能启动", err)
		}
	})
}
