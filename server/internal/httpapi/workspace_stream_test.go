package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
)

func TestFollowRunForwardsOnlyRelatedNestedDeliveryEvents(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	ctx := context.Background()
	bots := service.NewBots(db, events, t.TempDir())
	cat, err := bots.Create(ctx, service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	for index, turn := range []string{"root-turn", "child-turn", "other-turn"} {
		if _, err := db.Exec("INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,?,'user','succeeded',0)", turn, cat.ConversationID, index+1); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct{ id, turn, parent string }{{"root", "root-turn", ""}, {"child", "child-turn", "root"}, {"other", "other-turn", ""}} {
		if _, err := db.Exec("INSERT INTO runs(id,turn_id,conversation_id,bot_id,parent_run_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,NULLIF(?,''),'chat','succeeded','fixture','task',0)", entry.id, entry.turn, cat.ConversationID, cat.ID, entry.parent); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct{ kind, run string }{{"handoff.created", "child"}, {"user.message", "child"}, {"handoff.created", "other"}, {"user.message", "other"}} {
		if _, err := events.Publish(ctx, entry.kind, "another-conversation", map[string]string{"run_id": entry.run}); err != nil {
			t.Fatal(err)
		}
	}

	for _, entry := range []struct {
		kind, run, status, text string
		working                 bool
	}{
		{kind: "run.started", run: "root"},
		{kind: "run.activity", run: "root"},
		{kind: "run.status", run: "child", status: "queued"},
		{kind: "run.started", run: "child"},
		{kind: "run.progress", run: "child", text: "接收者正在核验"},
		{kind: "run.progress", run: "child"},
		{kind: "run.status", run: "child", status: "waiting_approval"},
		{kind: "run.status", run: "child", status: "running"},
		{kind: "run.status", run: "child", status: "failed"},
		{kind: "run.activity", run: "root", working: true},
		{kind: "run.status", run: "root", status: "canceled"},
		{kind: "run.started", run: "other"},
		{kind: "run.progress", run: "other", text: "其他回合的摘要"},
		{kind: "text.delta", run: "child", text: "子任务正文不能冒充父回复"},
	} {
		if _, err := events.Publish(ctx, entry.kind, "another-conversation", map[string]any{"run_id": entry.run, "bot_id": cat.ID, "status": entry.status, "text": entry.text, "is_working": entry.working}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := events.Publish(ctx, "turn.finished", cat.ConversationID, map[string]string{"turn_id": "root-turn", "status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Bus: events, Conversations: service.NewConversations(db, events)}
	var seen []string
	_, err = server.followRun(ctx, cat.ConversationID, "root-turn", 0, func(kind, payload string) error { seen = append(seen, kind+payload); return nil })
	if err != nil || len(seen) != 12 || !strings.HasPrefix(seen[0], "dm") || !strings.HasPrefix(seen[1], "user_message") || strings.Contains(strings.Join(seen, ""), "other") {
		t.Fatal("跨回合通知泄露或子任务通知缺失", seen, err)
	}
	expected := []struct {
		run               string
		working, finished bool
	}{
		{"root", true, false}, {"root", false, false}, {"child", false, false}, {"child", true, false},
		{"child", false, false}, {"child", true, false}, {"child", false, true}, {"root", true, false}, {"root", false, true},
	}
	index := 0
	for _, event := range seen {
		if !strings.HasPrefix(event, "activity") {
			continue
		}
		var payload struct {
			RunID    string `json:"run_id"`
			Working  bool   `json:"is_working"`
			Finished bool   `json:"is_finished"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "activity")), &payload); err != nil {
			t.Fatal(err)
		}
		want := expected[index]
		if payload.RunID != want.run || payload.Working != want.working || payload.Finished != want.finished {
			t.Fatal("运行活动顺序不正确", index, payload, want)
		}
		index++
	}
	if index != len(expected) || !strings.Contains(strings.Join(seen, ""), "接收者正在核验") || strings.Contains(strings.Join(seen, ""), "冒充") {
		t.Fatal("执行事件缺失或正文串线", seen)
	}

}

func TestFollowRunKeepsReplyWhenClientStopsReading(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	server := &Server{Bus: events}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go publishRun(t, ctx, events, "conv", "turn-1", "succeeded", "", "你", "好")
	var seen []string
	reply, err := server.followRun(ctx, "conv", "turn-1", 0, func(event, payload string) error {
		seen = append(seen, event+":"+payload)
		if event == "delta" {
			return errors.New("桌面已断开")
		}
		return nil
	})
	if err != nil || reply != "你好" {
		t.Fatalf("断开后应保留全文: %q %v %v", reply, err, seen)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "process") {
		t.Fatal("没有推送模型工作摘要", seen)
	}
}

func TestFollowRunFailureKeepsPartialReply(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	server := &Server{Bus: events}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go publishRun(t, ctx, events, "conv", "turn-2", "failed", "MODEL_FAILED", "说到一半")
	reply, err := server.followRun(ctx, "conv", "turn-2", 0, func(string, string) error { return nil })
	var failure *domain.Error
	if reply != "说到一半" || !errors.As(err, &failure) || failure.Code != "MODEL_FAILED" {
		t.Fatalf("失败时应带回已生成正文: %q %v", reply, err)
	}
}

func TestFollowRunSkipsDeltasFromEarlierTurns(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	server := &Server{Bus: events}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := events.Publish(ctx, "text.delta", "conv", map[string]string{"text": "你好"}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Publish(ctx, "turn.finished", "conv", map[string]any{"turn_id": "old", "status": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	cursor, err := server.eventCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go publishRun(t, ctx, events, "conv", "turn-new", "succeeded", "", "甲", "乙")
	reply, err := server.followRun(ctx, "conv", "turn-new", cursor, func(string, string) error { return nil })
	if err != nil || reply != "甲乙" {
		t.Fatalf("不应拼上上一轮回复: %q %v", reply, err)
	}
}

func publishRun(t *testing.T, ctx context.Context, events *bus.Bus, conversationID, turnID, status, code string, parts ...string) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if _, err := events.Publish(ctx, "thinking.delta", conversationID, map[string]string{"text": "在想"}); err != nil {
		t.Error(err)
	}
	if _, err := events.Publish(ctx, "run.progress", conversationID, map[string]string{"turn_id": turnID, "text": "正在整理资料"}); err != nil {
		t.Error(err)
	}
	if _, err := events.Publish(ctx, "tool.started", conversationID, map[string]string{}); err != nil {
		t.Error(err)
	}
	for _, part := range parts {
		if _, err := events.Publish(ctx, "text.delta", conversationID, map[string]string{"text": part}); err != nil {
			t.Error(err)
		}
	}
	if _, err := events.Publish(ctx, "turn.finished", conversationID, map[string]any{"turn_id": turnID, "status": status, "error_code": code}); err != nil {
		t.Error(err)
	}
}

func TestFollowRunOnlyPublishesCurrentPublicSummary(t *testing.T) {
	db := testkit.Database(t)
	events := bus.New(db)
	server := &Server{Bus: events}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, entry := range []struct {
		kind    string
		payload map[string]string
	}{
		{"thinking.delta", map[string]string{"text": "内部思考不可直接显示"}},
		{"run.progress", map[string]string{"turn_id": "old", "text": "旧摘要"}},
		{"run.progress", map[string]string{"turn_id": "current", "text": "正在核对资料"}},
		{"turn.finished", map[string]string{"turn_id": "current", "status": "succeeded"}},
	} {
		if _, err := events.Publish(ctx, entry.kind, "conv", entry.payload); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	_, err := server.followRun(ctx, "conv", "current", 0, func(kind, payload string) error { seen = append(seen, kind+payload); return nil })
	if err != nil || len(seen) != 1 || !strings.Contains(seen[0], "正在核对资料") {
		t.Fatal(seen, err)
	}
}
