package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

func TestInboxActivityFollowsMessagesAndTurnOwnership(t *testing.T) {
	ctx := t.Context()
	db := testkit.Database(t)
	events := bus.New(db)
	bot, err := NewBots(db, events, t.TempDir()).Create(ctx, BotInput{Template: "researcher"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	list := NewConversations(db, events)
	for seq, message := range []struct{ sender, blocks string }{
		{"bot", `[{"kind":"artifact","artifact":{"path":"out/report.md"}},{"kind":"tool_result","tool_result":{"artifact":{"path":"out/chart.jpg"}}}]`},
		{"user", `[{"kind":"artifact","artifact":{"path":"inbox/input.md"}}]`},
		{"bot", `[{"kind":"text","text":"artifact"}]`},
	} {
		if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES(?,?,?,?,?,0)`, testkit.Random(t), bot.ConversationID, seq+1, message.sender, message.blocks); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('work',?,1,'user','running',0)`, bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	item, err := list.Get(ctx, bot.ConversationID)
	if err != nil || item.UnreadArtifacts != 2 || !item.HasActiveTurn {
		t.Fatal("待办未按会话统计执行和未读产物", item, err)
	}
	if err = list.MarkRead(ctx, bot.ConversationID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE turns SET status='succeeded' WHERE id='work'"); err != nil {
		t.Fatal(err)
	}
	item, err = list.Get(ctx, bot.ConversationID)
	if err != nil || item.UnreadArtifacts != 0 || item.HasActiveTurn {
		t.Fatal("读取产物和结束回合后仍有待办", item, err)
	}
}

func TestConversationStablePaginationAndReadCursor(t *testing.T) {
	ctx := context.Background()
	db := testkit.Database(t)
	events := bus.New(db)
	bots := NewBots(db, events, t.TempDir())
	conversations := NewConversations(db, events)
	for _, template := range []string{"researcher", "writer", "assistant"} {
		if _, err := bots.Create(ctx, BotInput{Template: template}, "user"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE conversations SET updated_at=1000"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	var selected string
	for {
		page, err := conversations.List(ctx, "", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Items {
			if seen[c.ID] {
				t.Fatal("同毫秒分页重复")
			}
			seen[c.ID] = true
			selected = c.ID
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatal("同毫秒分页丢失记录")
	}
	for index, sender := range []string{"user", "bot", "system"} {
		if _, err := db.Exec(`INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES(?,?,?,?,'[{"kind":"text","text":"内容"}]',0)`, testkit.Random(t), selected, index+1, sender); err != nil {
			t.Fatal(err)
		}
	}
	c, err := conversations.Get(ctx, selected)
	if err != nil || c.UnreadCount != 2 {
		t.Fatal("未读不应包括用户消息")
	}
	if err = conversations.MarkRead(ctx, selected, 4); err == nil {
		t.Fatal("未来读取游标被接受")
	}
	if err = conversations.MarkRead(ctx, selected, 3); err != nil {
		t.Fatal(err)
	}
	if err = conversations.MarkRead(ctx, selected, 1); err != nil {
		t.Fatal(err)
	}
	c, err = conversations.Get(ctx, selected)
	if err != nil || c.LastReadSeq != 3 || c.UnreadCount != 0 {
		t.Fatal("读取游标发生回退")
	}
	messages, err := conversations.Messages(ctx, selected, 3, 2)
	if err != nil || len(messages) != 2 || messages[0].Seq != 1 || messages[1].Seq != 2 {
		t.Fatal("消息分页顺序错误")
	}
}

// 即使原工具消息在首屏以外，进入会话仍需还原当前步骤；历史参数只开放既定摘要字段。
func TestConversationActiveToolOutsideFirstPage(t *testing.T) {
	ctx := t.Context()
	db := testkit.Database(t)
	events := bus.New(db)
	bot, err := NewBots(db, events, t.TempDir()).Create(ctx, BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES('turn',?,1,'user','running',0)", bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES('run','turn',?,?,'chat','running','model','',0)", bot.ConversationID, bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES('call','run',4,'file_read','{"tool":"file_read","path":"notes/a","argument_bytes":8,"original":"不应发布的材料"}','running',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO messages(id,conversation_id,turn_id,run_id,seq,sender_kind,blocks_json,created_at) VALUES('use',?,'turn','run',1,'bot','[{"kind":"tool_use","tool_use":{"id":"call","name":"file_read","input":{}}}]',0)`, bot.ConversationID); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 51; i++ {
		if _, err = db.Exec("INSERT INTO messages(id,conversation_id,seq,sender_kind,blocks_json,created_at) VALUES(?,?,?,'system','[]',0)", fmt.Sprint(i), bot.ConversationID, i); err != nil {
			t.Fatal(err)
		}
	}
	s := NewConversations(db, events)
	snapshot, err := s.Snapshot(ctx, bot.ConversationID)
	if err != nil || len(snapshot.Messages) != 50 || snapshot.Messages[0].Seq != 2 || len(snapshot.ActiveRuns) != 1 {
		t.Fatal("未取得限定窗口", err)
	}
	active := snapshot.ActiveRuns[0].CurrentTool
	if active == nil || active.ID != "call" || active.Step != 4 {
		t.Fatal("当前步骤随分页边界丢失", active)
	}
	history, err := s.Messages(ctx, bot.ConversationID, 2, 1)
	if err != nil || len(history) != 1 || len(history[0].ToolCalls) != 1 {
		t.Fatal("历史调用信息缺失", err)
	}
	encoded, err := json.Marshal(history[0].ToolCalls)
	if err != nil || bytes.Contains(encoded, []byte("不应发布的材料")) || !bytes.Contains(encoded, []byte("notes/a")) {
		t.Fatal("历史摘要投影错误", err)
	}
}
