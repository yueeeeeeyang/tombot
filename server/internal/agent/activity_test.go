package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

func TestBotMessageActivityFollowsExecution(t *testing.T) {
	for _, mode := range []string{"reply", "no_reply", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			target := ""
			release := make(chan struct{})
			limits := DefaultLimits()
			limits.Concurrent = 1
			r, sender := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "Name: 接收猫咪\n") {
					select {
					case <-release:
					case <-ctx.Done():
						return provider.Usage{}, ctx.Err()
					}
					if mode == "failure" {
						return provider.Usage{}, errors.New("测试目标执行失败")
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "目标猫咪的结果"})
				}
				if lastToolResult(req) != nil {
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "发送者处理回执"})
				}
				return provider.Usage{}, emitMessageCall(emit, target, mode != "no_reply")
			}), limits)
			sender = enableMessages(t, r, sender)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer", Name: "接收猫咪"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			sent := submit(t, r, sender)
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				wake := r.deps.Events.Wake()
				states, _ := recordedActivities(t, r)
				if !states[sender.ID] && states[peer.ID] {
					break
				}
				select {
				case <-wake:
				case <-deadline.C:
					t.Fatal("发送者未停止或接收者未开始", states)
				}
			}
			if mode == "cancel" {
				if err := r.Cancel(context.Background(), sent.TurnID, "user"); err != nil {
					t.Fatal(err)
				}
			} else {
				close(release)
			}
			run := waitRun(t, r, sent.RunID)
			if mode == "cancel" && run.Status != "canceled" || mode != "cancel" && run.Status != "succeeded" {
				t.Fatal(run)
			}
			states, resumed := recordedActivities(t, r)
			if states[sender.ID] || states[peer.ID] {
				t.Fatal("结束后仍显示工作", states)
			}
			wantsResume := mode == "reply" || mode == "failure"
			if resumed[sender.ID] != wantsResume {
				t.Fatal("没有收到回执仍恢复，或收到回执未恢复", mode, resumed)
			}
		})
	}
}

// recordedActivities 用公开事件还原界面，确保状态携带实际执行者身份。
func recordedActivities(t *testing.T, r *Runtime) (map[string]bool, map[string]bool) {
	t.Helper()
	states, resumed := map[string]bool{}, map[string]bool{}
	var cursor int64
	for {
		events, reset, err := r.deps.Events.Read(context.Background(), cursor)
		if err != nil || reset {
			t.Fatal("无法读取执行事件", err, reset)
		}
		for _, event := range events {
			cursor = event.ID
			if event.Kind != "run.started" && event.Kind != "run.activity" && event.Kind != "run.status" {
				continue
			}
			var payload struct {
				BotID   string `json:"bot_id"`
				RunID   string `json:"run_id"`
				TurnID  string `json:"turn_id"`
				Status  string `json:"status"`
				Working bool   `json:"is_working"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.BotID == "" || payload.RunID == "" || payload.TurnID == "" {
				t.Fatal("执行事件缺少归属", event.Kind)
			}
			states[payload.BotID] = event.Kind == "run.started" || event.Kind == "run.activity" && payload.Working || event.Kind == "run.status" && payload.Status == "running"
			if event.Kind == "run.activity" && payload.Working {
				resumed[payload.BotID] = true
			}
		}
		if len(events) < 200 {
			return states, resumed
		}
	}
}
