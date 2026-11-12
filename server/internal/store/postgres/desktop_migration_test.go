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

func TestDesktopMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0041_" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	for _, populated := range []bool{false, true} {
		name := "空目录前向升级"
		if populated {
			name = "已有机器人拒绝转换且保留原记录"
		}
		t.Run(name, func(t *testing.T) {
			pool, _ := cluster.Database(t)
			if err := migrate(ctx, pool, previous); err != nil {
				t.Fatal(err)
			}
			if populated {
				userID := ids.New()
				if _, err := pool.Exec(`INSERT INTO tombot.users(id,username,display_name,role,status) VALUES($1,$2,'旧模型测试','user','provisioning')`, userID, strings.ToLower(userID)); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(`INSERT INTO tombot.bots(user_id,id,name) VALUES($1,$2,'旧模型机器人')`, userID, ids.New()); err != nil {
					t.Fatal(err)
				}
			}
			err := Migrate(ctx, pool)
			if !populated {
				if err != nil {
					t.Fatal("空目录不能升级", err)
				}
				if err = Migrate(ctx, pool); err != nil {
					t.Fatal("重复迁移失败", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "55000") {
				t.Fatal("旧目录未明确拒绝转换", err)
			}
			var count, version int
			var hasTable, hasColumn bool
			if err = pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.bots),(SELECT max(version) FROM tombot.schema_migrations),
				to_regclass('tombot.bot_desktops') IS NOT NULL,
				EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='tombot' AND table_name='bots' AND column_name='desktop_id')`).Scan(&count, &version, &hasTable, &hasColumn); err != nil {
				t.Fatal(err)
			}
			if count != 1 || version != 40 || hasTable || hasColumn {
				t.Fatal("拒绝转换后修改了已有数据或结构")
			}
		})
	}
}
