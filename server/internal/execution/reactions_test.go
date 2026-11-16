package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestMessageReactions(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("助手消息表情持久化并立即触发对应回复", func(t *testing.T) {
		f := newFixture(t, cluster)
		bot := f.plainBot(t)
		f.send(t, f.a, bot)
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if calls == 1 {
				return emitText(emit, "我已经整理好计划。")
			}
			last := input.Messages[len(input.Messages)-1]
			if last.Role != "user" || len(last.Blocks) != 2 || !strings.Contains(last.Blocks[0].Text, "Quoted sender: bot") || !strings.Contains(last.Blocks[0].Text, "我已经整理好计划。") || !strings.Contains(last.Blocks[1].Text, "added the 👍 reaction") {
				t.Error("模型未收到表情和目标原文")
			}
			return emitText(emit, "收到你的认可，我按这个计划继续。")
		}))
		if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" {
			t.Fatal(err)
		}
		page, err := f.service.Messages(ctx, f.a, bot.ConversationID, 0, 10)
		if err != nil || len(page) != 2 {
			t.Fatal(err)
		}
		request := ReactionRequest{ClientRequestID: ids.New(), Emoji: "👍", IsActive: true}
		marked, err := f.service.SetReaction(ctx, f.a, page[0].ID, request)
		if err != nil || marked.ReactionRevision != 1 || len(marked.Reactions) != 1 || marked.Reactions[0] != (Reaction{Emoji: "👍", Count: 1}) {
			t.Fatal("表情未落库", err)
		}
		f.service.options.ContextMessages = 1
		if run, err := worker.Process(ctx, f.claim(t)); err != nil || run.Status != "succeeded" || calls != 2 {
			t.Fatal("表情未触发回复", err)
		}
		visible, err := f.service.Messages(ctx, f.a, bot.ConversationID, 0, 10)
		if err != nil || len(visible) != 3 || visible[0].Blocks[0].Text != "收到你的认可，我按这个计划继续。" || visible[1].ReactionRevision != 1 {
			t.Fatal("历史丢失表情或混入交互占位", err)
		}
		for _, repeated := range []ReactionRequest{request, {ClientRequestID: ids.New(), Emoji: "👍", IsActive: true}} {
			if message, err := f.service.SetReaction(ctx, f.a, page[0].ID, repeated); err != nil || message.ReactionRevision != 1 {
				t.Fatal("重复操作修改了版本", err)
			}
		}
		_, err = f.service.Claim(ctx, ids.New())
		expect(t, err, ErrNoWork)
	})
	t.Run("运行中增删表情保留顺序和批次恢复且旧请求不恢复已撤销标记", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		lease := f.claim(t)
		work := f.start(t, lease)
		base := modelgateway.Input{Messages: []provider.Message{messageForModel(work.Messages[0])}}
		add := ReactionRequest{ClientRequestID: ids.New(), Emoji: "❤️", IsActive: true}
		marked, err := f.service.SetReaction(ctx, f.a, original.MessageID, add)
		if err != nil || len(marked.Reactions) != 1 {
			t.Fatal(err)
		}
		first, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		if err != nil || len(first.Messages) != 2 || !strings.Contains(first.Messages[1].Blocks[1].Text, "added the ❤️ reaction") {
			t.Fatal("执行中未收到表情", err)
		}
		remove := ReactionRequest{ClientRequestID: ids.New(), Emoji: "❤️", IsActive: false}
		removed, err := f.service.SetReaction(ctx, f.a, original.MessageID, remove)
		if err != nil || len(removed.Reactions) != 0 || removed.ReactionRevision != 2 {
			t.Fatal("撤销失败", err)
		}
		replayed, err := f.service.mergeRunInputs(ctx, lease, "model.1", base)
		if err != nil {
			t.Fatal(err)
		}
		firstBytes, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		replayedBytes, err := json.Marshal(replayed)
		if err != nil || string(firstBytes) != string(replayedBytes) {
			t.Fatal("恢复改变了原表情输入", err)
		}
		merged, err := f.service.mergeRunInputs(ctx, lease, "model.2", base)
		if err != nil || len(merged.Messages) != 3 || !strings.Contains(merged.Messages[2].Blocks[1].Text, "removed the ❤️ reaction") {
			t.Fatal("撤销事件没有按顺序进入模型", err)
		}
		stale, err := f.service.SetReaction(ctx, f.a, original.MessageID, add)
		if err != nil || len(stale.Reactions) != 0 || stale.ReactionRevision != 2 {
			t.Fatal("旧请求覆盖了后续撤销", err)
		}
		add.Emoji = "👎"
		_, err = f.service.SetReaction(ctx, f.a, original.MessageID, add)
		expect(t, err, ErrConflict)
		var inputs int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.run_inputs WHERE user_id=$1 AND turn_id=$2`, f.a.UserID, original.TurnID).Scan(&inputs); err != nil || inputs != 2 {
			t.Fatal("表情重复入队", err)
		}
	})
	t.Run("并发相同请求只保存一次并保持用户隔离", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		request := ReactionRequest{ClientRequestID: ids.New(), Emoji: "🎉", IsActive: true}
		var workers sync.WaitGroup
		for range 6 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				message, err := f.service.SetReaction(ctx, f.a, original.MessageID, request)
				if err != nil || message.ReactionRevision != 1 {
					t.Error("并发请求重复修改", err)
				}
			}()
		}
		workers.Wait()
		_, err := f.service.SetReaction(ctx, f.b, original.MessageID, request)
		expect(t, err, ErrNotFound)
		for _, invalid := range []ReactionRequest{{ClientRequestID: ids.New(), Emoji: "💩", IsActive: true}, {ClientRequestID: ids.New(), Emoji: "👍anything", IsActive: true}, {ClientRequestID: "invalid", Emoji: "👍", IsActive: true}} {
			_, err := f.service.SetReaction(ctx, f.a, original.MessageID, invalid)
			expect(t, err, ErrInvalidInput)
		}
		_, err = f.service.SetReaction(ctx, f.a, ids.New(), request)
		expect(t, err, ErrNotFound)
		var count int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.run_inputs WHERE user_id=$1 AND turn_id=$2`, f.a.UserID, original.TurnID).Scan(&count); err != nil || count != 1 {
			t.Fatal("并发创建了重复交互", err)
		}
		// 标记版本的更新授权不能扩展到正文，避免破坏引用与恢复时的原文。
		err = f.service.user(ctx, f.a, true, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE tombot.messages SET blocks='[]' WHERE user_id=$1 AND id=$2`, f.a.UserID, original.MessageID)
			return err
		})
		if err == nil {
			t.Fatal("表情授权允许了正文改写")
		}
	})
	t.Run("容量失败全部回滚且原请求可重试，内部交互不能继续标记", func(t *testing.T) {
		f := newFixture(t, cluster)
		original := f.send(t, f.a, f.first)
		request := ReactionRequest{ClientRequestID: ids.New(), Emoji: "🤔", IsActive: true}
		f.service.options.ContextMessages = 1
		_, err := f.service.SetReaction(ctx, f.a, original.MessageID, request)
		expect(t, err, ErrCapacity)
		var marks, receipts, revision int
		if err := f.pool.QueryRow(`SELECT (SELECT count(*) FROM tombot.message_reactions),(SELECT count(*) FROM tombot.message_reaction_requests),reaction_revision FROM tombot.messages WHERE user_id=$1 AND id=$2`, f.a.UserID, original.MessageID).Scan(&marks, &receipts, &revision); err != nil || marks != 0 || receipts != 0 || revision != 0 {
			t.Fatal("失败仍修改标记或去重回执", err)
		}
		f.service.options.ContextMessages = 20
		if _, err := f.service.SetReaction(ctx, f.a, original.MessageID, request); err != nil {
			t.Fatal(err)
		}
		var eventID string
		if err := f.pool.QueryRow(`SELECT message_id FROM tombot.run_inputs WHERE user_id=$1 AND client_request_id=$2`, f.a.UserID, request.ClientRequestID).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		_, err = f.service.SetReaction(ctx, f.a, eventID, ReactionRequest{ClientRequestID: ids.New(), Emoji: "👍", IsActive: true})
		expect(t, err, ErrNotFound)
		_, err = f.service.Send(ctx, f.a, f.first.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "引用内部交互", ReplyToID: eventID})
		expect(t, err, ErrInvalidInput)
	})
}
