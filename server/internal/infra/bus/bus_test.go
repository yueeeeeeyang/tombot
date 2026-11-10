package bus

import (
	"context"
	"testing"

	"io.yueyang/tom/internal/testkit"
)

func TestReplayRetentionWakeAndRestart(t *testing.T) {
	db := testkit.Database(t)
	events := New(db)
	ctx := context.Background()
	wake := events.Wake()
	var last int64
	for i := 0; i < Retention+2; i++ {
		id, err := events.Publish(ctx, "text.delta", "conversation", map[string]int{"index": i})
		if err != nil {
			t.Fatal(err)
		}
		if id <= last {
			t.Fatal("游标不单调")
		}
		last = id
	}
	select {
	case <-wake:
	default:
		t.Fatal("通知未唤醒")
	}
	if _, resync, err := events.Read(ctx, 1); err != nil || !resync {
		t.Fatalf("旧游标未要求同步: %v", err)
	}
	page, resync, err := events.Read(ctx, last-2)
	if err != nil || resync || len(page) != 2 {
		t.Fatalf("续传错误: %v", err)
	}
	restarted := New(db)
	next, err := restarted.Publish(ctx, "turn.finished", "conversation", map[string]bool{"ok": true})
	if err != nil || next <= last {
		t.Fatal("重启游标回退")
	}
	if _, resync, err = restarted.Read(ctx, next+1); err != nil || !resync {
		t.Fatal("未来游标未被拒绝")
	}
}
