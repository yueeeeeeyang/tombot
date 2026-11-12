package postgres

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestConnectionTargets(t *testing.T) {
	first := "postgres://identity@127.0.0.1:5432/platform?sslmode=disable"
	for _, fixture := range []struct {
		connections []string
		valid       bool
	}{
		{nil, false}, {[]string{first, "host=127.0.0.1 port=5432 user=tenant dbname=platform sslmode=disable"}, true},
		{[]string{first, "postgres://tenant@127.0.0.1:5432/other?sslmode=disable"}, false},
		{[]string{first, "postgres://tenant@localhost:5432/platform?sslmode=disable"}, false},
		{[]string{first, "postgres://tenant@127.0.0.1:5433/platform?sslmode=disable"}, false},
		{[]string{first, "host=127.0.0.1,127.0.0.2 port=5432 user=tenant dbname=platform sslmode=disable"}, false},
		{[]string{first, "postgres://%invalid"}, false},
	} {
		if err := ValidateSingleDatabase(fixture.connections); (err == nil) != fixture.valid {
			t.Fatalf("数据库目标判断错误: %v", err)
		}
	}
}

func TestRuntimeSchemaVerification(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, role := range []Role{IdentityRole, ControlRole, TenantRole, DispatchRole} {
		database := testDatabase(t, cluster, name, role)
		if err := database.VerifyCurrent(t.Context()); err != nil {
			t.Fatalf("用途 %s 无法只读核对版本: %v", role, err)
		}
		if err := database.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	database := testDatabase(t, cluster, name, IdentityRole)
	for _, statement := range []string{
		`DELETE FROM tombot.schema_migrations WHERE version=(SELECT max(version) FROM tombot.schema_migrations)`,
		`UPDATE tombot.schema_migrations SET checksum=repeat('0',64) WHERE version=29`,
		`INSERT INTO tombot.schema_migrations(version,name,checksum) VALUES(9999,'9999_unknown.sql',repeat('0',64))`,
	} {
		// 只改独立测试库账本并恢复内容；仓库历史迁移始终不可变。
		var version int64
		var name, checksum string
		if err := pool.QueryRow(`SELECT version,name,checksum FROM tombot.schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &name, &checksum); err != nil {
			t.Fatal(err)
		}
		var baseline string
		if err := pool.QueryRow(`SELECT checksum FROM tombot.schema_migrations WHERE version=29`).Scan(&baseline); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(statement); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyCurrent(t.Context()); err == nil {
			t.Error("接受不完整、篡改或未来版本账本")
		}
		if _, err := pool.Exec(`DELETE FROM tombot.schema_migrations WHERE version=9999;`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(`UPDATE tombot.schema_migrations SET checksum=$1 WHERE version=29`, baseline); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(`INSERT INTO tombot.schema_migrations(version,name,checksum) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, version, name, checksum); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyCurrent(t.Context()); err != nil {
			t.Fatal("核对改变了账本", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := database.VerifyCurrent(ctx); err == nil {
		t.Fatal("取消后仍核对成功")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Ping(t.Context()); err == nil {
		t.Fatal("已关闭的连接仍报告健康")
	}
}
