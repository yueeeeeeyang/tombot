package postgres

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMessageReplyForwardMigration(t *testing.T) {
	verifyMessageForwardMigration(t, "0066_")
}

func TestMessageReactionForwardMigration(t *testing.T) {
	verifyMessageForwardMigration(t, "0067_")
}

func verifyMessageForwardMigration(t *testing.T, firstNewVersion string) {
	t.Helper()
	cluster := pgtest.Start(t)
	pool, _ := cluster.Database(t)
	ctx := context.Background()
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= firstNewVersion {
			continue
		}
		encoded, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		previous["migrations/"+entry.Name()] = &fstest.MapFile{Data: encoded}
	}
	if err := migrate(ctx, pool, previous); err != nil {
		t.Fatal(err)
	}
	execute := func(query string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, query, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	userID, groupID, conversationID, turnID, messageID := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	digest := sha256.Sum256([]byte("迁移测试请求"))
	execute(`INSERT INTO tombot.users(id,username,display_name,status) VALUES($1,$2,'迁移用户','provisioning')`, userID, strings.ToLower(userID))
	execute(`INSERT INTO tombot.groups(user_id,id,name) VALUES($1,$2,'迁移群组')`, userID, groupID)
	execute(`INSERT INTO tombot.conversations(user_id,id,kind,group_id,title,message_sequence,turn_sequence) VALUES($1,$2,'group',$3,'迁移会话',1,1)`, userID, conversationID, groupID)
	execute(`INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,1,$4,$5,'chat','queued')`, userID, turnID, conversationID, ids.New(), digest[:])
	execute(`INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status) VALUES($1,$2,$3,$4,1,'user','[{"kind":"text","text":"历史正文"}]','complete')`, userID, messageID, conversationID, turnID)
	for range 2 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var replyTo *string
	var content string
	var reactionRevision int64
	if err := pool.QueryRow(`SELECT reply_to_id,blocks->0->>'text',reaction_revision FROM tombot.messages WHERE user_id=$1 AND id=$2`, userID, messageID).Scan(&replyTo, &content, &reactionRevision); err != nil || replyTo != nil || content != "历史正文" || reactionRevision != 0 {
		t.Fatal("迁移改变了历史消息", err)
	}
	execute(`INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status,reply_to_id) VALUES($1,$2,$3,$4,2,'user','[{"kind":"text","text":"新回复"}]','complete',$5)`, userID, ids.New(), conversationID, turnID, messageID)
	if _, err := pool.Exec(`DELETE FROM tombot.messages WHERE user_id=$1 AND id=$2`, userID, messageID); err == nil {
		t.Fatal("被引用原消息可以被删除")
	}
	if err := migrate(ctx, pool, previous); err == nil {
		t.Fatal("旧程序未拒绝降级启动")
	}
}
