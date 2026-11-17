package execution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestDirectBotDelivery(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, direct := range []bool{false, true} {
		name := "返回发起方后恢复工作"
		if direct {
			name = "接收方直接交付且发起方保持空闲"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.send(t, f.a, f.first)
			source := f.claim(t)
			f.start(t, source)
			result, invocation := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "查询明天上海的天气并直接发给用户", DeliverToUser: direct})
			if result.IsError {
				t.Fatal(result)
			}
			f.finishPrivateRun(t, source, "已交给同伴处理。")
			activity, err := f.service.BotActivities(ctx, f.a)
			if err != nil || len(activity) != 1 || activity[0].BotID != f.second.ID {
				t.Fatal("等待期间状态错误", activity, err)
			}
			peerActivity, err := f.service.Activity(ctx, f.a, f.second.ConversationID)
			if err != nil || len(peerActivity) != 1 || peerActivity[0].BotID != f.second.ID {
				t.Fatal("接收方主聊天未显示私信工作", peerActivity, err)
			}
			foreign, err := f.service.BotActivities(ctx, f.b)
			if err != nil || len(foreign) != 0 {
				t.Fatal("活动越权", foreign, err)
			}
			child := f.claim(t)
			work := f.start(t, child)
			if direct && !strings.Contains(work.PrivateContext, "automatic direct delivery enabled") {
				t.Fatal(work.PrivateContext)
			}
			messages, err := f.service.Messages(ctx, f.a, child.ConversationID, 0, 10)
			if err != nil || len(messages) != 1 || messages[0].Blocks[0].Text != "查询明天上海的天气并直接发给用户" || messages[0].Blocks[0].BotMessage.SenderID != f.first.ID {
				t.Fatal("私信请求在用户窗口不可见", messages, err)
			}
			if _, err = f.service.Messages(ctx, f.b, child.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
				t.Fatal("私信越权", err)
			}
			for _, bot := range []struct{ id, conversation, direction string }{{f.first.ID, f.first.ConversationID, "sent"}, {f.second.ID, f.second.ConversationID, "received"}} {
				page, err := f.service.Messages(ctx, f.a, bot.conversation, 0, 10)
				found := false
				for _, message := range page {
					if notice := message.Blocks[0].DMNotice; notice != nil {
						found = true
						if notice.ConversationID != child.ConversationID || notice.Direction != bot.direction {
							t.Fatal(notice)
						}
					}
				}
				if err != nil || !found {
					t.Fatal("没有私信入口", page, err)
				}
			}
			if direct {
				rejected, _ := f.privateCall(t, child, "send_user_message", botmessaging.UserMessage{Message: "重复交付"})
				if !rejected.IsError || !strings.Contains(rejected.Content, "AUTOMATIC_DELIVERY") {
					t.Fatal(rejected)
				}
			}
			f.finishPrivateRun(t, child, "上海明天天气查询结果。")
			counts := make(chan int, 2)
			failures := make(chan error, 2)
			var workers sync.WaitGroup
			for range 2 {
				workers.Go(func() { count, err := f.service.DeliverPrivateReplies(ctx, 100); counts <- count; failures <- err })
			}
			workers.Wait()
			if (<-counts)+(<-counts) != 1 {
				t.Fatal("并发重复投递")
			}
			for range 2 {
				if err := <-failures; err != nil {
					t.Fatal(err)
				}
			}
			if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 0 {
				t.Fatal("重复恢复", count, err)
			}
			var status string
			if err = f.pool.QueryRow(`SELECT status FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, f.a.UserID, invocation.OperationID).Scan(&status); err != nil || status != "delivered" {
				t.Fatal(status, err)
			}
			if direct {
				if _, err = f.service.Claim(ctx, ids.New()); !errors.Is(err, ErrNoWork) {
					t.Fatal("直接交付唤醒了发起方", err)
				}
				page, err := f.service.Messages(ctx, f.a, f.second.ConversationID, 0, 10)
				if err != nil || len(page) != 2 || !page[0].Blocks[0].UserMessage || page[0].Blocks[0].Text != "上海明天天气查询结果。" {
					t.Fatal(page, err)
				}
				if _, err = f.service.Send(ctx, f.a, f.second.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "谢谢，请补充建议", ReplyToID: page[0].ID}); err != nil {
					t.Fatal("交付结果不可回复", err)
				}
				followup := f.start(t, f.claim(t))
				for _, message := range followup.Messages {
					if message.Blocks[0].DMNotice != nil {
						t.Fatal("私信展示提示进入了模型上下文")
					}
				}
			} else {
				activity, err = f.service.BotActivities(ctx, f.a)
				if err != nil || len(activity) != 1 || activity[0].BotID != f.first.ID {
					t.Fatal("收到回复后未恢复工作", activity, err)
				}
			}
		})
	}
	t.Run("直接交付失败与取消保留明确结果", func(t *testing.T) {
		for _, cancelSource := range []bool{false, true} {
			f := newFixture(t, cluster)
			f.send(t, f.a, f.first)
			source := f.claim(t)
			f.start(t, source)
			result, _ := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "核对", DeliverToUser: true})
			if result.IsError {
				t.Fatal(result)
			}
			if cancelSource {
				if _, err := f.service.Cancel(ctx, f.a, source.RunID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.Complete(ctx, source, Outcome{Status: "canceled", ErrorCode: "CANCELED"}); err != nil {
					t.Fatal(err)
				}
			} else {
				f.finishPrivateRun(t, source, "已交付")
			}
			child := f.claim(t)
			if _, err := f.service.Complete(ctx, child, Outcome{Status: "failed", ErrorCode: "EXECUTION_FAILED"}); err != nil {
				t.Fatal(err)
			}
			count, err := f.service.DeliverPrivateReplies(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			page, err := f.service.Messages(ctx, f.a, f.second.ConversationID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if cancelSource {
				if count != 0 || len(page) != 1 {
					t.Fatal("取消后仍投递", count, page)
				}
			} else {
				if count != 1 || !strings.Contains(page[0].Blocks[0].Text, "未完成") {
					t.Fatal("失败被报告为成功", count, page)
				}
			}
		}
	})
}

func TestProactiveUserMessage(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("主动发信验证输入且回执恢复不重复", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		source := f.claim(t)
		f.start(t, source)
		for _, input := range []any{botmessaging.UserMessage{}, botmessaging.UserMessage{Message: strings.Repeat("a", 32769)}, map[string]string{"message": "越权", "bot_id": f.foreign.ID}} {
			result, _ := f.privateCall(t, source, "send_user_message", input)
			if !result.IsError {
				t.Fatal("接受无效输入", result)
			}
		}
		result, invocation := f.privateCall(t, source, "send_user_message", botmessaging.UserMessage{Message: "主动通知"})
		if result.IsError {
			t.Fatal(result)
		}
		// 已提交操作再次执行必须拒绝；恢复器读取既有回执而不重发消息。
		tool := botMessageTool{executor: f.service, schema: botmessaging.Schemas()[2], maximum: 16384}
		if _, err := tool.Execute(ctx, invocation); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		page, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || len(page) != 2 || !page[0].Blocks[0].UserMessage {
			t.Fatal(page, err)
		}
		if messageForModel(page[0]).Role != "assistant" {
			t.Fatal("主动通知被误当成用户请求")
		}
		execute(t, f.pool, `UPDATE tombot.conversations SET is_archived=true WHERE user_id=$1 AND id=$2`, f.a.UserID, f.first.ConversationID)
		if _, err := tool.Execute(ctx, invocation); err == nil {
			t.Fatal("归档后仍允许发送")
		}
	})
}
