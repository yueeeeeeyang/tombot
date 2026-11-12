package store

import (
	"context"
	"testing"
)

func TestEqualGroupMigrationKeepsHistoryAndFreezesInitialPortraits(t *testing.T) {
	db := databaseBeforeDesktopColumns(t)
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		migration, err := loadMigration(file)
		if err != nil {
			t.Fatal(err)
		}
		if migration.version < 20 || migration.version >= 27 {
			continue
		}
		if _, err := db.Exec(string(migration.script)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES(?,?,?,0)", migration.version, migration.name, migration.checksum); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at,avatar_preset) VALUES('cat','小猫','职责','身份','bots/cat',1,1,1,'calico');INSERT INTO groups(id,name,coordinator_bot_id,created_at,updated_at) VALUES('group','旧群','cat',1,1);INSERT INTO group_members VALUES('group','cat','协调',1);INSERT INTO conversations(id,kind,source,group_id,title,summary,summary_upto_seq,created_at,updated_at) VALUES('conversation','group','user','group','旧群','旧共用摘要',1,1,1);INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,status,created_at) VALUES('message','conversation',1,'user','[{"kind":"text","text":"旧消息"}]','complete',1)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	var avatars, summary, body, audience string
	if err := db.QueryRow("SELECT avatar_presets_json FROM groups WHERE id='group'").Scan(&avatars); err != nil || avatars != `["calico"]` {
		t.Fatal(avatars, err)
	}
	if err := db.QueryRow("SELECT summary FROM conversations WHERE id='conversation'").Scan(&summary); err != nil || summary != "" {
		t.Fatal("不可信旧摘要没有清除", summary, err)
	}
	if err := db.QueryRow("SELECT blocks_json,audience_json FROM messages WHERE id='message'").Scan(&body, &audience); err != nil || body != `[{"kind":"text","text":"旧消息"}]` || audience != "[]" {
		t.Fatal("旧消息内容或公开范围改变", body, audience, err)
	}
	if _, err := db.Exec("SELECT coordinator_bot_id FROM groups"); err == nil {
		t.Fatal("仍保留旧协调配置")
	}
	if _, err := db.Exec("DELETE FROM group_members WHERE group_id='group'"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT avatar_presets_json FROM groups WHERE id='group'").Scan(&avatars); err != nil || avatars != `["calico"]` {
		t.Fatal("成员变化重置了头像", avatars, err)
	}
}
