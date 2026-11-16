package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMessageReplies(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()

	t.Run("新回合持久化引用且单条分页仍返回摘要", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		if _, err := f.service.Cancel(ctx, f.a, original.RunID); err != nil {
			t.Fatal(err)
		}
		request := SendRequest{ClientRequestID: ids.New(), Content: "继续这个问题", ReplyToID: original.MessageID}
		sent, err := f.service.Send(ctx, f.a, f.first.ConversationID, request)
		if err != nil {
			t.Fatal(err)
		}
		page, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 1)
		if err != nil || len(page) != 1 || page[0].ReplyTo == nil || page[0].ReplyTo.MessageID != original.MessageID || page[0].ReplyTo.Excerpt != "请检查这项任务。" || page[0].Blocks[0].Text != request.Content {
			t.Fatal("重读消息丢失引用或污染正文", err)
		}
		replay, err := f.service.Send(ctx, f.a, f.first.ConversationID, request)
		if err != nil || replay.IsCreated || replay.MessageID != sent.MessageID {
			t.Fatal("引用消息重试不幂等", err)
		}
		request.ReplyToID = ""
		_, err = f.service.Send(ctx, f.a, f.first.ConversationID, request)
		expect(t, err, ErrConflict)
	})
	t.Run("模型看到历史窗口之外的完整助手原文", func(t *testing.T) {
		f := newFixture(t, cluster)
		bot := f.plainBot(t)
		f.send(t, f.a, bot)
		originalText := strings.Repeat("这是较长的原文。", 14) + "最后一条需要特别核对。"
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				return emitText(emit, originalText)
			}
			if len(input.Messages) != 1 || input.Messages[0].Role != "user" || len(input.Messages[0].Blocks) != 3 || !strings.Contains(input.Messages[0].Blocks[0].Text, originalText) || !strings.Contains(input.Messages[0].Blocks[0].Text, "Quoted sender: bot") || input.Messages[0].Blocks[1].Text != "请解释最后一条" || !strings.Contains(input.Messages[0].Blocks[2].Text, "Source message ID") {
				t.Error("模型没有得到明确回复关系与完整原文")
			}
			return emitText(emit, "这是最后一条的解释。")
		}))
		if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" {
			t.Fatal("原文生成失败", err)
		}
		page, err := f.service.Messages(ctx, f.a, bot.ConversationID, 0, 1)
		if err != nil || len(page) != 1 {
			t.Fatal(err)
		}
		if _, err := f.service.Send(ctx, f.a, bot.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "请解释最后一条", ReplyToID: page[0].ID}); err != nil {
			t.Fatal(err)
		}
		f.service.options.ContextMessages = 1
		if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" || calls != 2 {
			t.Fatal("回复执行失败", err)
		}
	})
	t.Run("运行中补充引用在模型边界加入且重放不变", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		lease := f.claim(t)
		f.start(t, lease)
		request := SendRequest{ClientRequestID: ids.New(), Content: "补充说明", ReplyToID: original.MessageID}
		if _, err := f.service.Send(ctx, f.a, f.first.ConversationID, request); err != nil {
			t.Fatal(err)
		}
		base := modelgateway.Input{Messages: []provider.Message{{Role: "assistant"}}}
		merged, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		if err != nil || len(merged.Messages) != 2 || !strings.Contains(merged.Messages[1].Blocks[0].Text, original.MessageID) || !strings.Contains(merged.Messages[1].Blocks[0].Text, "请检查这项任务。") || merged.Messages[1].Blocks[1].Text != request.Content {
			t.Fatal("补充引用丢失", err)
		}
		page, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 1)
		if err != nil || page[0].ReplyTo == nil {
			t.Fatal("补充引用未保存", err)
		}
		replayed, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		first, firstErr := json.Marshal(merged)
		second, secondErr := json.Marshal(replayed)
		if err != nil || firstErr != nil || secondErr != nil || string(first) != string(second) {
			t.Fatal("重放上下文改变", err)
		}
	})
	t.Run("引用越界或不存在时不创建消息且数据库拒绝伪造", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		other := f.send(t, f.a, f.second)
		foreign := f.send(t, f.b, f.foreign)
		for _, target := range []string{"invalid", ids.New(), other.MessageID, foreign.MessageID} {
			_, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "无效引用", ReplyToID: target})
			expect(t, err, ErrInvalidInput)
		}
		messages, err := f.service.Messages(ctx, f.a, f.first.ConversationID, 0, 10)
		if err != nil || len(messages) != 1 {
			t.Fatal("拒绝后仍创建消息", err)
		}
		for _, target := range []string{other.MessageID, foreign.MessageID, original.MessageID} {
			// 相同序号不能反向构造未来引用，跨会话和跨租户也必须由数据库兜住。
			_, err := f.pool.Exec(`INSERT INTO tombot.messages(user_id,id,conversation_id,turn_id,sequence,sender_kind,blocks,status,reply_to_id) VALUES($1,$2,$3,$4,1,'user','[{"kind":"text","text":"伪造"}]','complete',$5)`, f.a.UserID, ids.New(), f.first.ConversationID, original.TurnID, target)
			if err == nil {
				t.Fatal("数据库接受了无效引用")
			}
		}
	})
	t.Run("原文计入上下文预算不静默截断", func(t *testing.T) {
		f := newFixture(t, cluster)
		original, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: strings.Repeat("a", 2048)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Cancel(ctx, f.a, original.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "引用", ReplyToID: original.MessageID}); err != nil {
			t.Fatal(err)
		}
		f.service.options.ContextBytes = 1024
		_, err = f.service.Start(ctx, f.claim(t))
		expect(t, err, ErrContextLimit)
		_, err = f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "追加引用", ReplyToID: original.MessageID})
		expect(t, err, ErrCapacity)
	})
}

func TestReplyExcerpt(t *testing.T) {
	reply := &MessageReply{}
	encoded, err := json.Marshal([]Block{{Kind: "text", Text: " \n" + strings.Repeat("🐈", 81) + "\t"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeReply(reply, encoded); err != nil {
		t.Fatal(err)
	}
	if len([]rune(reply.Excerpt)) != replyExcerptLength || !strings.HasSuffix(reply.Excerpt, "…") || len(reply.blocks[0].Text) <= len(reply.Excerpt) {
		t.Fatal("摘要截断损坏字符或原文")
	}
	if err := decodeReply(reply, []byte(`invalid`)); err == nil {
		t.Fatal("无效引用原文被忽略")
	}
}
