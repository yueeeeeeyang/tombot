package service

import (
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

func TestNotificationSnapshotIncludesDefaultAssistantConversation(t *testing.T) {
	s, _, _ := approvalFixture(t)
	ctx := t.Context()
	bots := NewBots(s.db, s.events, t.TempDir())
	if err := bots.EnsureDefault(ctx); err != nil {
		t.Fatal("默认助手初始化失败", err)
	}
	bot, err := bots.Get(ctx, DefaultBotID)
	if err != nil || bot.ConversationID != "tom-direct" {
		t.Fatal("默认助手会话标识异常", bot.ConversationID, err)
	}
	turnID := ids.New()
	finishedAt := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO turns(id,conversation_id,seq,source,status,created_at,finished_at) VALUES(?,?,1,'user','succeeded',?,?)`, turnID, bot.ConversationID, finishedAt, finishedAt); err != nil {
		t.Fatal("默认助手完成回合创建失败", err)
	}
	snapshot, err := s.NotificationStatus(ctx)
	want := CompletedTurn{ID: turnID, ConversationID: "tom-direct", FinishedAt: finishedAt}
	if err != nil || snapshot.PendingCount != 0 || snapshot.PendingApprovalID != nil || snapshot.CompletedTurn == nil || *snapshot.CompletedTurn != want {
		t.Fatal("默认助手完成回合未保留真实会话标识", snapshot, err)
	}
}

func TestNotificationSnapshotFiltersAndOrders(t *testing.T) {
	s, run, _ := approvalFixture(t)
	ctx := t.Context()
	snapshot, err := s.NotificationStatus(ctx)
	if err != nil || snapshot.PendingCount != 0 || snapshot.PendingApprovalID != nil || snapshot.CompletedTurn != nil {
		t.Fatal("初始通知快照异常", snapshot, err)
	}
	for i, row := range []struct {
		id, source, status string
		finished           int64
	}{
		{"first", "user", "succeeded", 10}, {"last", "routine", "succeeded", 10},
		{"failed", "user", "failed", 11}, {"child", "bot_message", "succeeded", 12},
	} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO turns(id,conversation_id,seq,source,status,created_at,finished_at) VALUES(?,?,?,?,?,0,?)", row.id, run.ConversationID, i+2, row.source, row.status, row.finished); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		id, status string
		expiry     int64
	}{
		{"a", "pending", time.Now().Add(time.Hour).UnixMilli()}, {"b", "pending", 0}, {"c", "approved", time.Now().Add(time.Hour).UnixMilli()},
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id,action_class,title,detail_json,risk_level,status,expires_at,created_at) VALUES(?,'routine.create','','{}','low',?,?,0)`, item.id, item.status, item.expiry); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err = s.NotificationStatus(ctx)
	want := CompletedTurn{ID: "last", ConversationID: run.ConversationID, FinishedAt: 10}
	if err != nil || snapshot.PendingCount != 1 || snapshot.PendingApprovalID == nil || *snapshot.PendingApprovalID != "a" || snapshot.CompletedTurn == nil || *snapshot.CompletedTurn != want {
		t.Fatal("过期审批、子回合或排序异常", snapshot, err)
	}
	items, err := s.List(ctx, "pending", "", 50)
	if err != nil || len(items) != 1 || items[0].ID != "a" {
		t.Fatal("审批页应排除已过期的决定", items, err)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+notificationSnapshot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"idx_notification_pending", "idx_notification_completed"} {
		if !strings.Contains(strings.Join(plan, "\n"), index) {
			t.Fatal("通知查询未使用索引", plan)
		}
	}
}
