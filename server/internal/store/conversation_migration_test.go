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

func TestConversationMigrationRebuildsPublicIndexAndRollsBackInvalidHistory(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
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
				if version >= 14 {
					continue
				}
				script, err := migrations.ReadFile("migrations/" + file.Name())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(strings.Replace(string(script), "CREATE TABLE schema_migrations (", "CREATE TABLE IF NOT EXISTS schema_migrations (", 1)); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?,0)`, version, file.Name(), fmt.Sprintf("%x", sha256.Sum256(script))); err != nil {
					t.Fatal(err)
				}
			}
			blocks := `[{"kind":"text","text":"公开历史正文"},{"kind":"tool_result","tool_result":{"tool_call_id":"call","content":"私有历史载荷"}},{"kind":"note","text":"公开状态提示"}]`
			if invalid {
				blocks = "invalid history"
			}
			if _, err = db.Exec(`INSERT INTO conversations(id,kind,title,created_at,updated_at) VALUES('conversation','direct','历史会话',0,0)`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES('message','conversation',1,'user',?,0)`, blocks); err != nil {
				t.Fatal(err)
			}
			err = Migrate(context.Background(), db)
			var version int
			if scanErr := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); scanErr != nil {
				t.Fatal(scanErr)
			}
			if invalid {
				if err == nil || version != 13 {
					t.Fatal("坏历史未使升级完整回滚", version, err)
				}
				var old string
				if err = db.QueryRow(`SELECT content FROM messages_fts WHERE message_id='message'`).Scan(&old); err != nil || old != blocks {
					t.Fatal("回滚丢失原索引", old, err)
				}
				return
			}
			if err != nil || version != 19 {
				t.Fatal(version, err)
			}
			var content, original string
			if err = db.QueryRow(`SELECT f.content,m.blocks_json FROM messages_fts f JOIN messages m ON m.id=f.message_id WHERE m.id='message'`).Scan(&content, &original); err != nil || content != "公开历史正文\n公开状态提示" || original != blocks {
				t.Fatal("索引未清除私有内容或修改了历史消息", content, err)
			}
			if err = Migrate(context.Background(), db); err != nil {
				t.Fatal("重复启动不幂等", err)
			}
		})
	}
}
