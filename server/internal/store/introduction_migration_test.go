package store

import (
	"context"
	"testing"
)

func TestIntroductionMigrationSeparatesDesktopRoleAndCascadesCache(t *testing.T) {
	db := databaseBeforeDesktopColumns(t)
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		next, err := loadMigration(file)
		if err != nil {
			t.Fatal(err)
		}
		if next.version < 20 || next.version >= 26 {
			continue
		}
		if _, err := db.Exec(string(next.script)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES(?,?,?,0)", next.version, next.name, next.checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at,desktop_id) VALUES('desktop','猫咪','旧介绍','身份','bots/desktop',1,1,1,'cat'),('native','研究员','研究职责','身份','bots/native',1,1,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	var role string
	if err := db.QueryRow("SELECT role_title FROM bots WHERE id='desktop'").Scan(&role); err != nil || role != "助手" {
		t.Fatal("旧桌面介绍仍控制工作", role, err)
	}
	if err := db.QueryRow("SELECT role_title FROM bots WHERE id='native'").Scan(&role); err != nil || role != "研究职责" {
		t.Fatal("改动了非桌面角色", role, err)
	}
	if _, err := db.Exec("INSERT INTO bot_introductions(bot_id,text) VALUES('desktop','新的展示介绍')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM bots WHERE id='desktop'"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM bot_introductions").Scan(&count); err != nil || count != 0 {
		t.Fatal("删除猫咪未清除介绍", count, err)
	}
	if _, err := db.Exec("INSERT INTO bot_introductions(bot_id) VALUES('missing')"); err == nil {
		t.Fatal("接受了不存在猫咪的介绍")
	}
}
