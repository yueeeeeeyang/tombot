package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestDesktopStateMovesIntoBotColumns(t *testing.T) {
	db := databaseBeforeDesktopColumns(t)
	if _, err := db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at) VALUES('bot-1','旧名','职责','soul','bots/bot-1',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversations(id,kind,source,bot_id,title,created_at,updated_at) VALUES('conv-1','direct','user','bot-1','旧名',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO desktop_agent_link(agent_id,bot_id,conversation_id) VALUES('assistant','bot-1','conv-1')`); err != nil {
		t.Fatal(err)
	}
	workspace := `{"version":5,"activeAgentId":"assistant","agents":[{"id":"assistant","name":"小黑","description":"日常搭档","group":"我的助手","avatar":"bombay","modelId":"gpt-4o","messages":[{"id":"m1","role":"user","content":"你好","createdAt":"2026-11-10T00:00:00Z","status":"sent","reactions":[{"emoji":"❤️","count":1}],"replyTo":null}],"draft":"还在写","artifacts":[{"id":"a1","title":"草稿","kind":"file","createdAt":"2026-11-10T00:00:00Z"}],"computerUrl":"https://computer.example.test"}]}`
	settings := `{"language":"zh-CN","provider":"openai","baseUrl":"https://example.test/v1","modelName":"gpt-4o","temperature":0.7,"topP":1,"maxOutputTokens":"4096","isStreaming":true}`
	if _, err := db.Exec(`INSERT INTO desktop_state(id,workspace_json,revision,pristine,settings_json,updated_at) VALUES(1,?,4,0,?,10)`, workspace, settings); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var name, group, draft, model, computer, desktopID string
	if err := db.QueryRow(`SELECT name, group_name, draft, catalog_model_id, computer_url, desktop_id FROM bots WHERE id='bot-1'`).Scan(&name, &group, &draft, &model, &computer, &desktopID); err != nil {
		t.Fatal(err)
	}
	if name != "小黑" || group != "我的助手" || draft != "还在写" || model != "gpt-4o" || computer != "https://computer.example.test" || desktopID != "assistant" {
		t.Fatalf("猫咪字段没有迁到 bots: %s %s %s %s %s %s", name, group, draft, model, computer, desktopID)
	}
	var content, status string
	var reactions int
	if err := db.QueryRow(`SELECT blocks_json, status FROM messages WHERE id='m1'`).Scan(&content, &status); err != nil || !strings.Contains(content, "你好") || status != "sent" {
		t.Fatal("消息没有迁到 messages", content, status, err)
	}
	if err := db.QueryRow(`SELECT count FROM message_reactions WHERE message_id='m1' AND emoji='❤️'`).Scan(&reactions); err != nil || reactions != 1 {
		t.Fatal("表情没有迁到 message_reactions", err, reactions)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM bot_artifacts WHERE id='a1' AND bot_id='bot-1'`).Scan(&title); err != nil || title != "草稿" {
		t.Fatal("资料库没有迁到 bot_artifacts", err, title)
	}
	var active, modelName string
	if err := db.QueryRow(`SELECT active_agent_id, model_name FROM workspace_meta WHERE id=1`).Scan(&active, &modelName); err != nil || active != "assistant" || modelName != "gpt-4o" {
		t.Fatal("工作空间元数据没有迁走", err, active, modelName)
	}
	if _, err := db.Exec(`SELECT 1 FROM desktop_state`); err == nil {
		t.Fatal("迁完后不应再保留 desktop_state")
	}
}

func databaseBeforeDesktopColumns(t *testing.T) *sql.DB {
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
		if next.version >= 20 {
			continue
		}
		if _, err = db.Exec(strings.Replace(string(next.script), "CREATE TABLE schema_migrations (", "CREATE TABLE IF NOT EXISTS schema_migrations (", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?,0)`, next.version, next.name, next.checksum); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
