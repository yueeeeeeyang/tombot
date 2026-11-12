package postgres

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestDesktopControlMigrationPreservesOldLimits(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	ctx := context.Background()
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0042_" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	if err = migrate(ctx, pool, previous); err != nil {
		t.Fatal(err)
	}
	userID := ids.New()
	if _, err = pool.Exec(`INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'旧限额测试','user','provisioning')`, userID, strings.ToLower(userID)); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(`INSERT INTO tombot.user_limits(user_id,concurrent_model_calls,queued_runs) VALUES($1,3,5)`, userID); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "55000") {
		t.Fatal("旧限额被隐式转换", err)
	}
	var maximum, version int
	var hasColumn bool
	if err = pool.QueryRow(`SELECT concurrent_model_calls,(SELECT max(version) FROM tombot.schema_migrations),
	EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='tombot' AND table_name='user_limits' AND column_name='concurrent_desktops')
	FROM tombot.user_limits WHERE user_id=$1`, userID).Scan(&maximum, &version, &hasColumn); err != nil || maximum != 3 || version != 41 || hasColumn {
		t.Fatal("拒绝转换后改写旧限额或数据库结构", err)
	}
}
