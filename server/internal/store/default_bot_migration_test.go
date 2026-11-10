package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func databaseBeforeDefaultBot(t *testing.T) *sql.DB {
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
		if next.version >= 17 {
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

func readDefaultBotPending(t *testing.T, db *sql.DB) (string, int64) {
	t.Helper()
	var encoded string
	var updatedAt int64
	if err := db.QueryRow(`SELECT value_json,updated_at FROM system_state WHERE key='default_bot.pending'`).Scan(&encoded, &updatedAt); err != nil {
		t.Fatal(err)
	}
	var pending struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		Name           string `json:"name"`
		Role           string `json:"role_title"`
		Soul           string `json:"soul"`
		Directory      string `json:"workspace_dir"`
		Hue            int    `json:"accent_hue"`
		Tools          struct {
			Allowed  []string `json:"allowed_tools"`
			Approval string   `json:"approval"`
		} `json:"tool_policy"`
		Messaging struct {
			AcceptDM    bool `json:"accept_dm"`
			CanInitiate bool `json:"can_initiate"`
			AcceptGroup bool `json:"accept_group_invite"`
		} `json:"messaging_policy"`
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		t.Fatal("待创建配置格式错误，模型必须留待服务消费时选择", err)
	}
	if pending.ID != "tom" || pending.ConversationID != "tom-direct" || pending.Name != "Tom" || pending.Role != "通用助手" || pending.Directory != "bots/tom" || pending.Hue != 18 || updatedAt <= 0 {
		t.Fatal("默认助手待创建配置不完整", pending, updatedAt)
	}
	if !strings.HasPrefix(pending.Soul, "You are Tom, a general-purpose assistant.") || !strings.Contains(pending.Soul, "request approval before high-risk actions") {
		t.Fatal("默认助手提示词缺少角色或审批边界", pending.Soul)
	}
	expectedTools := []string{"file_read", "file_list", "file_glob", "file_search", "memory_search", "memory_write", "memory_delete", "conversation_search", "conversation_read", "ask_user"}
	if !reflect.DeepEqual(pending.Tools.Allowed, expectedTools) || pending.Tools.Approval != "standard" || !pending.Messaging.AcceptDM || pending.Messaging.CanInitiate || !pending.Messaging.AcceptGroup {
		t.Fatal("默认助手工具或消息策略错误", pending)
	}
	return encoded, updatedAt
}

func TestDefaultBotMigrationRecordsPendingWithoutBusinessData(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "新数据库", true: "已有用户配置"}[existing], func(t *testing.T) {
			var db *sql.DB
			if existing {
				db = databaseBeforeDefaultBot(t)
				if _, err := db.Exec(`INSERT INTO providers(id,kind,display_name,created_at) VALUES('provider','openai_compat','现有供应商',0);
INSERT INTO models(id,provider_id,model_key,display_name,context_window,max_output,input_price_micro,output_price_micro,is_default,created_at) VALUES('model','provider','existing','现有模型',8192,1024,0,0,1,0);
INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at) VALUES('existing','Tom','用户职责','User instructions.','bots/existing',120,10,20),('second','Tom 1','用户职责','User instructions.','bots/second',240,10,20);`); err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				db, err = Open(context.Background(), filepath.Join(t.TempDir(), "tom.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if err := Migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			encoded, updatedAt := readDefaultBotPending(t, db)
			var bots, conversations int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM bots),(SELECT COUNT(*) FROM conversations)`).Scan(&bots, &conversations); err != nil {
				t.Fatal(err)
			}
			expectedBots := 0
			if existing {
				expectedBots = 2
				var soul string
				if err := db.QueryRow(`SELECT soul FROM bots WHERE id='existing' AND name='Tom' AND updated_at=20`).Scan(&soul); err != nil || soul != "User instructions." {
					t.Fatal("迁移修改了已有用户 Bot", err)
				}
			}
			if bots != expectedBots || conversations != 0 {
				t.Fatal("迁移提前创建了业务数据", bots, conversations)
			}
			if err := Migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			repeated, repeatedAt := readDefaultBotPending(t, db)
			if encoded != repeated || updatedAt != repeatedAt {
				t.Fatal("重复迁移修改了待创建配置")
			}
			if _, err := db.Exec(`DELETE FROM system_state WHERE key='default_bot.pending'`); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM system_state WHERE key='default_bot.pending'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("已消费的待创建配置被重新生成", count, err)
			}
		})
	}
}

func TestDefaultBotMigrationLeavesReservedIdentityConflictsForService(t *testing.T) {
	db := databaseBeforeDefaultBot(t)
	if _, err := db.Exec(`INSERT INTO bots(id,name,role_title,soul,workspace_dir,accent_hue,created_at,updated_at) VALUES('tom','现有助手','用户职责','User instructions.','bots/existing',120,10,20);
INSERT INTO conversations(id,kind,bot_id,title,created_at,updated_at) VALUES('tom-direct','direct','tom','已有会话',10,20)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal("保留标识冲突应由服务消费配置时处理", err)
	}
	readDefaultBotPending(t, db)
	var count, version int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bots b JOIN conversations c ON c.bot_id=b.id WHERE b.id='tom' AND b.name='现有助手' AND b.soul='User instructions.' AND b.updated_at=20 AND c.id='tom-direct' AND c.title='已有会话' AND c.updated_at=20`).Scan(&count); err != nil || count != 1 {
		t.Fatal("迁移覆盖了冲突的用户业务数据", count, err)
	}
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 18 {
		t.Fatal("迁移账本未完成升级", version, err)
	}
}
