package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestLiveChatInputs(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()

	t.Run("历史携带每条用户消息的请求标识并保持租户隔离", func(t *testing.T) {
		f := newFixture(t, cluster)
		first := SendRequest{ClientRequestID: ids.New(), Content: "第一条"}
		second := SendRequest{ClientRequestID: ids.New(), Content: "第二条"}
		for _, request := range []SendRequest{first, second} {
			if _, err := f.service.Send(ctx, f.a, f.first.ConversationID, request); err != nil {
				t.Fatal("发送失败", err)
			}
		}
		messages, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 2 || messages[0].ClientRequestID != second.ClientRequestID || messages[1].ClientRequestID != first.ClientRequestID {
			t.Fatal("历史未保留各自的发送标识", err)
		}
		older, err := f.service.Messages(ctx, f.a, f.first.ConversationID, messages[0].Sequence, 1)
		if err != nil || len(older) != 1 || older[0].ClientRequestID != first.ClientRequestID {
			t.Fatal("分页丢失发送标识", err)
		}
		_, err = f.service.Messages(ctx, f.b, f.first.ConversationID, 0, 10)
		expect(t, err, ErrNotFound)
	})
	t.Run("工具间隙读取新消息且恢复批次不变", func(t *testing.T) {
		f := newFixture(t, cluster)
		initial := f.send(t, f.a, f.first)
		lease := f.claim(t)
		f.start(t, lease)
		base := modelgateway.Input{Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "原任务"}}}}}
		first, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		if err != nil || len(first.Messages) != 1 {
			t.Fatal("初始上下文错误", err)
		}
		request := SendRequest{ClientRequestID: ids.New(), Content: "改为只检查第二份文件"}
		added, err := f.service.Send(ctx, f.a, f.first.ConversationID, request)
		if err != nil || added.RunID != initial.RunID {
			t.Fatal("补充消息受理失败", err)
		}
		repeat, err := f.service.Send(ctx, f.a, f.first.ConversationID, request)
		if err != nil || repeat.IsCreated || repeat.MessageID != added.MessageID {
			t.Fatal("补充消息幂等失败", err)
		}
		_, err = f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: request.ClientRequestID, Content: "变更正文"})
		expect(t, err, ErrConflict)
		replay, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		if err != nil || len(replay.Messages) != 1 {
			t.Fatal("恢复改变了已执行步骤", err)
		}
		next := base
		next.Messages = append(next.Messages, provider.Message{Role: "assistant"}, provider.Message{Role: "tool"})
		merged, err := f.service.mergeRunInputs(ctx, lease, "model.2", next)
		if err != nil || len(merged.Messages) != 4 || merged.Messages[2].Role != "tool" || merged.Messages[3].Blocks[0].Text != request.Content {
			t.Fatal("工具边界未追加用户消息", err)
		}
		next.Messages = append(next.Messages, provider.Message{Role: "assistant"}, provider.Message{Role: "tool"})
		merged, err = f.service.mergeRunInputs(ctx, lease, "model.3", next)
		if err != nil || len(merged.Messages) != 6 || merged.Messages[3].Blocks[0].Text != request.Content || merged.Messages[5].Role != "tool" {
			t.Fatal("后续模型丢失补充上下文", err)
		}
		_, err = f.service.Activity(ctx, f.b, f.first.ConversationID)
		expect(t, err, ErrNotFound)
		_, err = f.service.mergeRunInputs(ctx, lease, "model.0", base)
		expect(t, err, ErrInvalidInput)
		for i := 1; i < f.service.options.ContextMessages-1; i++ {
			if _, err = f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "追加"}); err != nil {
				t.Fatal(err)
			}
		}
		_, err = f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "超出容量"})
		expect(t, err, ErrCapacity)
	})
	t.Run("最终模型调用期间到达的消息自动继续执行", func(t *testing.T) {
		f := newFixture(t, cluster)
		bot := f.plainBot(t)
		initial := f.send(t, f.a, bot)
		lease := f.claim(t)
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				added, err := f.service.Send(ctx, f.a, bot.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "再告诉我核对方法"})
				if err != nil || added.RunID != initial.RunID {
					t.Error("运行中无法接收", err)
				}
				return emitText(emit, "第一项已核对。")
			}
			last := input.Messages[len(input.Messages)-1]
			if last.Role != "user" || last.Blocks[0].Text != "再告诉我核对方法" || !strings.Contains(input.System, "have not been answered yet") {
				t.Error("续跑未识别未回答消息")
			}
			return emitText(emit, "核对方法如下。")
		}))
		if run, err := worker.Process(ctx, lease); err != nil || run.Status != "succeeded" {
			t.Fatal("首轮失败", err)
		}
		active, err := f.service.Activity(ctx, f.a, bot.ConversationID)
		if err != nil || len(active) != 1 || active[0].ID == initial.RunID || active[0].Status != "queued" {
			t.Fatal("收尾丢失补充消息", err)
		}
		next := f.claim(t)
		if run, err := worker.Process(ctx, next); err != nil || run.Status != "succeeded" || calls != 2 {
			t.Fatal("续跑失败", err)
		}
		active, err = f.service.Activity(ctx, f.a, bot.ConversationID)
		if err != nil || len(active) != 0 {
			t.Fatal("结束后仍显示工作中", err)
		}
		if _, err = f.service.Claim(ctx, ids.New()); !errors.Is(err, ErrNoWork) {
			t.Fatal("重复创建续跑", err)
		}
	})
	t.Run("工作摘要经真实工具调用持久化并发布", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		f.send(t, f.a, f.first)
		lease := f.claim(t)
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				found := false
				for _, tool := range input.Tools {
					if tool.Name == "report_progress" {
						found = true
					}
				}
				if !found || !strings.Contains(input.System, "Progress updates") {
					t.Error("没有接通进度工具")
				}
				args := `{"summary":"正在检查第二份文件"}`
				for _, event := range []provider.Event{{Kind: "tool_use_start", Index: 0, ToolCallID: "progress-call", ToolName: "report_progress"}, {Kind: "tool_use_end", Index: 0, ToolCallID: "progress-call", ToolName: "report_progress", InputDelta: args}, {Kind: "stop", StopReason: "tool_calls"}} {
					if err := emit(event); err != nil {
						return modelgateway.Result{}, err
					}
				}
				return modelgateway.Result{IsComplete: true, Usage: reported(10, 2)}, nil
			}
			active, err := f.service.Activity(ctx, f.a, f.first.ConversationID)
			if err != nil || len(active) != 1 || active[0].ProgressSummary != "正在检查第二份文件" {
				t.Error("摘要未持久化", err)
			}
			return emitText(emit, "文件已检查。")
		}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
			t.Error("不应调用文件工具")
			return botagent.ToolResult{}, ErrInvalidInput
		}})
		if run, err := worker.Process(ctx, lease); err != nil || run.Status != "succeeded" || calls != 2 {
			t.Fatal("摘要工具运行失败", run.Status, err)
		}
		var payload []byte
		if err := f.pool.QueryRow(`SELECT payload FROM tombot.user_events WHERE user_id=$1 AND kind='run.progress' AND resource_id=$2`, lease.UserID, lease.RunID).Scan(&payload); err != nil {
			t.Fatal("缺少进度事件", err)
		}
		var event map[string]string
		if err := json.Unmarshal(payload, &event); err != nil || event["bot_id"] != lease.BotID || event["text"] != "正在检查第二份文件" {
			t.Fatal("进度事件未绑定机器人", err)
		}
		invalid, err := (progressTool{executor: f.service}).Execute(ctx, Invocation{Lease: lease, Call: botagent.ToolCall{Arguments: `{"summary":"a\nb"}`}})
		if err != nil || !invalid.IsError {
			t.Fatal("无效摘要未拒绝", err)
		}
	})
}
