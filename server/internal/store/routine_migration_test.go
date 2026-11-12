package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestRoutineMigrationPreservesApprovalsAndMainConversations(t *testing.T) {
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
		version, err := strconv.Atoi(strings.Split(file.Name(), "_")[0])
		if err != nil {
			t.Fatal(err)
		}
		if version >= 12 {
			continue
		}
		script, err := migrations.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(strings.Replace(string(script), "CREATE TABLE schema_migrations (", "CREATE TABLE IF NOT EXISTS schema_migrations (", 1)); err != nil {
			t.Fatal(file.Name(), err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?,0)`, version, file.Name(), fmt.Sprintf("%x", sha256.Sum256(script))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at) VALUES('bot','旧同事','职责','System role','bots/bot',0,0,0);
INSERT INTO conversations(id,kind,bot_id,title,created_at,updated_at) VALUES('main','direct','bot','主会话',0,0);
INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('turn','main',1,'user','waiting_approval',0);
INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES('run','turn','main','bot','chat','waiting_approval','model','request',0);
INSERT INTO approvals(id,run_id,tool_call_id,bot_id,action_class,title,detail_json,risk_level,status,expires_at,created_at) VALUES('approval','run','call','bot','fs.write','旧审批','{}','medium','pending',100,0);`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var approvals int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='approvals'`).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatal("审批表还在", approvals, err)
	}
	if _, err = db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('routine','direct','routine','bot','例程会话',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('duplicate','direct','user','bot','重复主会话',0,0)`); err == nil {
		t.Fatal("主会话唯一约束被移除")
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("迁移破坏外键")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
