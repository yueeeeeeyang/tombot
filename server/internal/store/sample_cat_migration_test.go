package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestUntouchedSampleCatsAreRemovedAndRealCatsStay(t *testing.T) {
	db := databaseBefore(t, 22)
	if _, err := db.Exec(`INSERT INTO workspace_meta(id,active_agent_id,revision,pristine,language,provider,base_url,model_name,temperature,top_p,max_output_tokens,is_streaming,updated_at) VALUES(1,'developer',1,1,'system','openai','','',0.7,1,'4096',1,1)`); err != nil {
		t.Fatal(err)
	}
	insertCat(t, db, "developer", "developer", "游戏程序", "代码、逻辑和实现，交给我一起想。", "游戏开发", "orange-tabby", "", "")
	insertCat(t, db, "coding", "coding", "编程搭档", "聊聊代码，也聊聊下一步怎么做。", "未分组", "american-shorthair", "", "")
	insertCat(t, db, "renamed", "assistant", "我家的猫", "你的日常搭档，陪你把想法变成现实。", "我的助手", "bombay", "还在写", "")
	insertCat(t, db, "mine", "mine-cat", "阿花", "自己招聘的", "未分组", "calico", "", "")
	if _, err := db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('conv-coding','direct','user','coding','编程搭档',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES('m1','conv-coding',1,'user','[{"kind":"text","text":"你好"}]',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('conv-developer','direct','user','developer','游戏程序',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM bots WHERE id='developer'`) != 0 {
		t.Fatal("未改动的样例猫咪应被删除")
	}
	for _, id := range []string{"coding", "renamed", "mine"} {
		if countRows(t, db, `SELECT COUNT(*) FROM bots WHERE id='`+id+`'`) != 1 {
			t.Fatal("真实或已使用的猫咪被删掉", id)
		}
	}
	var active string
	if err := db.QueryRow(`SELECT active_agent_id FROM workspace_meta WHERE id=1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active == "developer" || active == "" {
		t.Fatal("当前猫咪应改到仍存在的猫咪", active)
	}
}

func TestEarliestCatBecomesLeadWhenNoAssistant(t *testing.T) {
	db := databaseBefore(t, 25)
	if _, err := db.Exec(`INSERT INTO workspace_meta(id,active_agent_id,revision,pristine,language,provider,provider_name,base_url,model_name,updated_at) VALUES(1,'late',1,0,'system','custom','','','',1)`); err != nil {
		t.Fatal(err)
	}
	insertCat(t, db, "late", "late", "后到的", "第二只", "未分组", "bombay", "", "")
	insertCat(t, db, "early", "early", "先到的", "第一只", "未分组", "calico", "", "")
	if _, err := db.Exec(`UPDATE bots SET created_at=1 WHERE id='early'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE bots SET created_at=2 WHERE id='late'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var lead string
	if err := db.QueryRow(`SELECT lead_agent_id FROM workspace_meta WHERE id=1`).Scan(&lead); err != nil || lead != "early" {
		t.Fatal("没有默认猫时应置顶最早创建的猫", lead, err)
	}
}

func insertCat(t *testing.T, db *sql.DB, id, desktopID, name, description, group, avatar, draft, computer string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at,desktop_id,group_name,description,avatar_preset,catalog_model_id,draft,computer_url) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, name, "助手", "soul", "bots/"+id, 1, 1, 1, desktopID, group, description, avatar, "gpt-4o", draft, computer); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func databaseBefore(t *testing.T, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = db.Exec(`PRAGMA foreign_keys=ON;CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		next, err := loadMigration(file)
		if err != nil {
			t.Fatal(err)
		}
		if next.version >= version {
			continue
		}
		if _, err = db.Exec(strings.Replace(string(next.script), "CREATE TABLE schema_migrations (", "CREATE TABLE IF NOT EXISTS schema_migrations (", 1)); err != nil {
			t.Fatal(file.Name(), err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?,0)`, next.version, next.name, next.checksum); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
