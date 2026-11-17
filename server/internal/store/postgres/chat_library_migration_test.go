package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestChatLibraryForwardMigration(t *testing.T) {
	cluster := pgtest.Start(t)
	pool, name := cluster.Database(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0074_" {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: content}
	}
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []Role{TenantRole, IdentityRole, ControlRole, DispatchRole} {
		execute(`DO $roles$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='` + string(role) + `') THEN CREATE ROLE ` + string(role) + ` NOLOGIN; END IF; END $roles$`)
	}
	owner := cluster.Login(t)
	execute(`GRANT CREATE ON DATABASE ` + name + ` TO ` + owner)
	migration := pgtest.Connect(t, cluster.DSN(name, owner))
	if err = migrate(t.Context(), migration, previous); err != nil {
		t.Fatal(err)
	}
	type stored struct{ user, conversation, file, draft, message string }
	records := []stored{}
	body := []byte("历史文件内容")
	digest := sha256.Sum256(body)
	for range 2 {
		user, group, conversation, turn, message, file, draft := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
		execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'迁移用户','provisioning')`, user, strings.ToLower(user))
		execute(`INSERT INTO tombot.groups(user_id,id,name) VALUES($1,$2,'迁移群')`, user, group)
		execute(`INSERT INTO tombot.conversations(user_id,id,kind,group_id,title,message_sequence,turn_sequence) VALUES($1,$2,'group',$3,'群聊',1,1)`, user, conversation, group)
		for _, id := range []string{file, draft} {
			execute(`INSERT INTO tombot.group_files(user_id,id,group_id,name,media_type,size,sha256,creation_digest) VALUES($1,$2,$3,'改名后.txt','text/plain',$4,$5,$6)`, user, id, group, len(body), hex.EncodeToString(digest[:]), digest[:])
			execute(`INSERT INTO tombot.group_file_contents(user_id,file_id,content) VALUES($1,$2,$3)`, user, id, body)
		}
		execute(`INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,1,$4,$5,'chat','queued')`, user, turn, conversation, ids.New(), digest[:])
		blocks, err := json.Marshal([]any{map[string]any{"kind": "text", "text": "历史正文"}, map[string]any{"kind": "group_file", "group_file": map[string]any{"id": file, "group_id": group, "name": "原文件名.txt", "sha256": hex.EncodeToString(digest[:])}}})
		if err != nil {
			t.Fatal(err)
		}
		execute(`INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,1,'user',$5,'complete')`, user, message, conversation, turn, blocks)
		records = append(records, stored{user, conversation, file, draft, message})
	}
	for range 2 {
		if err = Migrate(t.Context(), migration); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range records {
		var conversation, content, historicalName, kind string
		var shared, draftShared bool
		if err = pool.QueryRow(`SELECT f.conversation_id,f.is_shared,c.content,(SELECT is_shared FROM tombot.chat_files WHERE user_id=$1 AND id=$3) FROM tombot.chat_files f JOIN tombot.chat_file_contents c ON c.user_id=f.user_id AND c.file_id=f.id WHERE f.user_id=$1 AND f.id=$2`, record.user, record.file, record.draft).Scan(&conversation, &shared, &content, &draftShared); err != nil {
			t.Fatal(err)
		}
		if conversation != record.conversation || !shared || draftShared || content != string(body) {
			t.Fatal("文件归属、可见性或内容迁移错误")
		}
		if err = pool.QueryRow(`SELECT blocks->1->>'kind',blocks->1->'chat_file'->>'name',blocks->1->'chat_file'->>'conversation_id' FROM tombot.messages WHERE user_id=$1 AND id=$2`, record.user, record.message).Scan(&kind, &historicalName, &conversation); err != nil {
			t.Fatal(err)
		}
		if kind != "chat_file" || historicalName != "原文件名.txt" || conversation != record.conversation {
			t.Fatal("历史附件快照被破坏")
		}
	}
	var policies int
	if err = pool.QueryRow(`SELECT count(*) FROM pg_policies WHERE schemaname='tombot' AND policyname='chat_library_migration'`).Scan(&policies); err != nil || policies != 0 {
		t.Fatal("迁移权限未回收", err)
	}
	if err = migrate(t.Context(), migration, previous); err == nil {
		t.Fatal("旧版本没有拒绝降级启动")
	}
}
