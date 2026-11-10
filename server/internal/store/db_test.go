package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"io.yueyang/tom/internal/store"
)

func TestMigrationIntegrityAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tom.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil || count != 24 {
		t.Fatalf("迁移数量: %d %v", count, err)
	}
	if _, err = db.Exec(`INSERT INTO models(id,provider_id,model_key,display_name,context_window,max_output,input_price_micro,output_price_micro,created_at) VALUES('m','missing','m','模型',1024,256,0,0,0)`); err == nil {
		t.Fatal("外键未生效")
	}
	if _, err = db.Exec(`INSERT INTO routines(id,name,cron,prompt,created_at) VALUES('r','例程','* * * * *','test',0)`); err == nil {
		t.Fatal("例程空目标必须被拒绝")
	}
	if _, err = db.Exec(`UPDATE schema_migrations SET checksum='changed' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, db); err == nil {
		t.Fatal("历史迁移篡改必须阻断启动")
	}
}

func TestMessageFTSTriggers(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = db.Exec(`INSERT INTO conversations(id,kind,title,created_at,updated_at) VALUES('c','direct','测试',0,0); INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES('m','c',1,'user','[{"kind":"text","text":"山巅中文检索"}]',0)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '中文检索'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("中文检索失败: %d %v", count, err)
	}
	if _, err = db.Exec(`UPDATE messages SET blocks_json='[{"kind":"text","text":"新的内容"}]' WHERE id='m'`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '中文检索'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("旧索引未清理: %d %v", count, err)
	}
	if _, err = db.Exec(`DELETE FROM messages WHERE id='m'`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("删除索引失败: %d %v", count, err)
	}
}
