package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestTypingReplyDelivery(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, scenario := range []struct {
		name    string
		isGroup bool
		failure string
		status  string
	}{
		{"单聊完整发送", false, "", "succeeded"},
		{"群聊完整发送", true, "", "succeeded"},
		{"单聊中断不显示残片", false, "disconnect", "unknown"},
		{"群聊中断不显示残片", true, "disconnect", "unknown"},
		{"单聊截断不显示残片", false, "length", "failed"},
		{"群聊截断不显示残片", true, "length", "failed"},
		{"单聊取消不发送正文", false, "cancel", "canceled"},
		{"群聊取消不发送正文", true, "cancel", "canceled"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.toolsModel(t)
			conversation := f.first.ConversationID
			if scenario.isGroup {
				conversation = f.group(t).ConversationID
			}
			request := SendRequest{ClientRequestID: ids.New(), Content: "帮我核对一下"}
			if scenario.isGroup {
				request.Mentions = []string{f.first.ID}
			}
			if _, err := f.service.Send(ctx, f.a, conversation, request); err != nil {
				t.Fatal(err)
			}
			lease := f.claim(t)
			worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				if err := emit(provider.Event{Kind: "thinking_delta", Text: "内部推理不展示"}); err != nil {
					return modelgateway.Result{}, err
				}
				active, err := f.service.Activity(ctx, f.a, conversation)
				if err != nil || len(active) != 1 || active[0].ProgressSummary == typingSummary {
					t.Fatal("思考阶段误报输入", active, err)
				}
				for _, part := range []string{"资料已经核对，", "可以直接使用。"} {
					if err := emit(provider.Event{Kind: "text_delta", Text: part}); err != nil {
						return modelgateway.Result{}, err
					}
					active, err = f.service.Activity(ctx, f.a, conversation)
					if err != nil || len(active) != 1 || active[0].ProgressSummary != typingSummary {
						t.Fatal("输入状态未持久化", active, err)
					}
					messages, err := f.service.Messages(ctx, f.a, conversation, 0, 20)
					if err != nil || len(messages) != 1 || messages[0].SenderKind != "user" {
						t.Fatal("完成前泄露正文片段", messages, err)
					}
				}
				if scenario.failure == "disconnect" {
					return modelgateway.Result{}, errors.New("测试上游中断")
				}
				stopReason := "stop"
				if scenario.failure == "length" {
					stopReason = "length"
				}
				if scenario.failure == "cancel" {
					// 取消与模型最后一个片段同时到达时，收尾也不能发送已经撤回的回复。
					if _, err := f.service.Cancel(ctx, f.a, lease.RunID); err != nil {
						t.Fatal(err)
					}
				}
				if err := emit(provider.Event{Kind: "stop", StopReason: stopReason}); err != nil {
					return modelgateway.Result{}, err
				}
				return modelgateway.Result{IsComplete: true, Usage: reported(10, 10)}, nil
			}), testTool{})
			run, err := worker.Process(ctx, lease)
			if err != nil || run.Status != scenario.status {
				t.Fatal(run, err)
			}
			active, err := f.service.Activity(ctx, f.a, conversation)
			if err != nil || len(active) != 0 {
				t.Fatal("收尾后仍显示输入", active, err)
			}
			messages, err := f.service.Messages(ctx, f.a, conversation, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			answers := 0
			for _, message := range messages {
				for _, block := range message.Blocks {
					if strings.Contains(block.Text, "资料已经核对") {
						answers++
						if scenario.failure != "" || block.Text != "资料已经核对，可以直接使用。" {
							t.Fatal("发送了不完整正文", block.Text)
						}
					}
				}
			}
			if scenario.failure == "" && answers != 1 {
				t.Fatal("完整回复未恰好显示一次", answers)
			}
			if scenario.failure != "" {
				var evidence string
				if err = f.pool.QueryRow(`SELECT result->>'text' FROM tombot.run_operations WHERE user_id=$1 AND run_id=$2 AND kind='model'`, lease.UserID, lease.RunID).Scan(&evidence); err != nil || evidence != "资料已经核对，可以直接使用。" {
					t.Fatal("中断模型证据丢失", evidence, err)
				}
			}
			kind := "run.progress"
			if scenario.isGroup {
				kind = "group." + kind
			}
			var count int
			if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.user_events WHERE user_id=$1 AND resource_id=$2 AND kind=$3 AND payload->>'text'=$4`, lease.UserID, lease.RunID, kind, typingSummary).Scan(&count); err != nil || count != 1 {
				t.Fatal("每个片段重复发布状态", count, err)
			}
			if err = f.service.setReplyTyping(ctx, lease, true); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("旧租约重新标记输入", err)
			}
		})
	}
}

func TestTypingReturnsToWorkForToolCalls(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	f := newFixture(t, cluster)
	f.toolsModel(t)
	f.send(t, f.a, f.first)
	lease := f.claim(t)
	calls := 0
	worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
		calls++
		if calls == 1 {
			if err := emit(provider.Event{Kind: "text_delta", Text: "需要先核对文件"}); err != nil {
				return modelgateway.Result{}, err
			}
			return emitTool(emit, `{"path":"report.txt"}`)
		}
		return emitText(emit, "已经核对，内容正确。")
	}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
		active, err := f.service.Activity(ctx, f.a, f.first.ConversationID)
		if err != nil || len(active) != 1 || active[0].ProgressSummary == typingSummary {
			t.Fatal("执行工具时仍显示输入", active, err)
		}
		return botagent.ToolResult{Content: "Verified."}, nil
	}})
	if run, err := worker.Process(ctx, lease); err != nil || run.Status != "succeeded" || calls != 2 {
		t.Fatal(run, err)
	}
	messages, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 20)
	if err != nil || len(messages) != 2 || messages[0].Blocks[0].Text != "已经核对，内容正确。" {
		t.Fatal("工具前言变成聊天消息", messages, err)
	}
}
