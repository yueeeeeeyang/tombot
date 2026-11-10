package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/testkit"
)

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
		t.Fatal("没有推送思考或工具过程", seen)
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
