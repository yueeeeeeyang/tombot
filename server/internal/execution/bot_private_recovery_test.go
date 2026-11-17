package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/botmessaging"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func TestBotPrivateFallback(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	for _, alternative := range []bool{true, false} {
		name := "无合适同伴时自己执行"
		if alternative {
			name = "忙碌后协调其他空闲同伴"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, cluster)
			f.toolsModel(t)
			var other bots.Bot
			if alternative {
				other = f.extraCat(t, "空闲同伴")
			}
			f.send(t, f.a, f.first)
			lease := f.claim(t)
			f.send(t, f.a, f.second)
			calls, ownWork := 0, 0
			worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
				calls++
				switch calls {
				case 1:
					return emitMemory(emit, "send_bot_message", memoryArguments(t, botmessaging.Send{BotID: f.second.ID, Message: "核对文件", RequiredTools: []string{"file_read"}}))
				case 2:
					result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
					if result == nil || !result.IsError || !strings.Contains(result.Content, "BOT_BUSY") || !strings.Contains(result.Content, "do the work yourself") {
						t.Fatal("缺少明确协调策略", result)
					}
					if alternative {
						if !strings.Contains(result.Content, other.ID) {
							t.Fatal("未返回合适候选")
						}
						return emitMemory(emit, "send_bot_message", memoryArguments(t, botmessaging.Send{BotID: other.ID, Message: "核对文件", RequiredTools: []string{"file_read"}}))
					}
					if !strings.Contains(result.Content, `"bots":[]`) {
						t.Fatal("错误的空闲候选")
					}
					return emitTool(emit, `{"path":"report.txt"}`)
				default:
					result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult
					if result == nil || result.IsError {
						t.Fatal(result)
					}
					return emitText(emit, "已按当前可用能力处理。")
				}
			}), testTool{execute: func(context.Context, Invocation) (botagent.ToolResult, error) {
				ownWork++
				return botagent.ToolResult{Content: "Verified file contents."}, nil
			}})
			run, err := worker.Process(ctx, lease)
			if err != nil || run.Status != "succeeded" {
				t.Fatal(run, err)
			}
			var count int
			if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if alternative && (count != 1 || ownWork != 0) || !alternative && (count != 0 || ownWork != 1) {
				t.Fatal("协调分支错误", count, ownWork)
			}
		})
	}
}

func TestBotPrivateRecovery(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("工具回执失败不留下私信且重放不重复受理", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		lease := f.claim(t)
		f.start(t, lease)
		call := botagent.ToolCall{ID: ids.New(), Name: "send_bot_message", Arguments: memoryArguments(t, botmessaging.Send{BotID: f.second.ID, Message: "核对", RequiredTools: []string{}})}
		encoded, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(encoded)
		request := OperationRequest{StepKey: "private.atomic", Kind: "tool", ToolName: call.Name, RequestDigest: digest[:]}
		operation, _, err := f.service.BeginOperation(ctx, lease, request)
		if err != nil {
			t.Fatal(err)
		}
		tool := botMessageTool{executor: f.service, schema: botmessaging.Schemas()[1], maximum: 16384}
		invocation := Invocation{Lease: lease, OperationID: operation.ID, Call: call}
		execute(t, f.pool, `CREATE FUNCTION tombot.reject_private_receipt() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN IF NEW.kind='tool' AND NEW.status='succeeded' THEN RAISE EXCEPTION '回执写入失败'; END IF; RETURN NEW; END $body$`)
		execute(t, f.pool, `CREATE TRIGGER reject_private_receipt BEFORE UPDATE ON tombot.run_operations FOR EACH ROW EXECUTE FUNCTION tombot.reject_private_receipt()`)
		if result, err := tool.Execute(ctx, invocation); err == nil {
			t.Fatal("保存失败仍报告成功", result)
		}
		var count int
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil || count != 0 {
			t.Fatal("留下部分私信", count, err)
		}
		execute(t, f.pool, `DROP TRIGGER reject_private_receipt ON tombot.run_operations`)
		if result, err := tool.Execute(ctx, invocation); err != nil || result.IsError {
			t.Fatal(result, err)
		}
		execute(t, f.pool, `UPDATE tombot.jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND id=$2`, lease.UserID, lease.JobID)
		if count, err = f.service.Recover(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		// 接收方先入队，直接令其排队时间推后，使恢复后的发送方先领取。
		execute(t, f.pool, `UPDATE tombot.jobs SET available_at=CASE WHEN id=$2 THEN clock_timestamp()-interval '1 minute' ELSE clock_timestamp()+interval '1 minute' END WHERE user_id=$1 AND status='queued'`, lease.UserID, lease.JobID)
		resumed := f.claim(t)
		work := f.start(t, resumed)
		replay, created, err := f.service.BeginOperation(ctx, resumed, request)
		if err != nil || created || replay.Status != "succeeded" {
			t.Fatal(replay, created, err)
		}
		if !strings.Contains(work.BotDirectory, f.second.ID) {
			t.Fatal("恢复丢失目录快照")
		}
		if err = f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil || count != 1 {
			t.Fatal("恢复重复受理", count, err)
		}
		if _, err = tool.Execute(ctx, invocation); err == nil {
			t.Fatal("旧租约可以再次发信")
		}
	})
	t.Run("群里发出的私信回复只唤醒发起猫咪", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		if _, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "请协作", Mentions: []string{f.first.ID}}); err != nil {
			t.Fatal(err)
		}
		source := f.claim(t)
		f.start(t, source)
		result, _ := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "核对文件"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, source, "已安排")
		target := f.claim(t)
		f.start(t, target)
		f.finishPrivateRun(t, target, "文件正确")
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		resumed := f.claim(t)
		if resumed.BotID != f.first.ID || resumed.ConversationID != group.ConversationID {
			t.Fatal("没有回到原群的发起猫咪")
		}
		work := f.start(t, resumed)
		hiddenID := work.Messages[len(work.Messages)-1].ID
		if work.Messages[len(work.Messages)-1].Blocks[0].BotMessage == nil {
			t.Fatal("发起猫咪没有收到私信结果")
		}
		f.finishPrivateRun(t, resumed, "核对完成")
		if _, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "引用内部消息", ReplyToID: hiddenID}); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("允许通过引用读取私信", err)
		}
		if _, err := f.service.SetReaction(ctx, f.a, hiddenID, ReactionRequest{ClientRequestID: ids.New(), Emoji: "👍", IsActive: true}); !errors.Is(err, ErrNotFound) {
			t.Fatal("允许对内部私信添加表情", err)
		}
		messages, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || messages[0].ReplyTo != nil {
			t.Fatal("群回复不应引用私信原文", err)
		}
		for _, message := range messages {
			for _, block := range message.Blocks {
				if block.BotMessage != nil {
					t.Fatal("私信泄露到群历史")
				}
			}
		}
		if err = f.tenant.Tenant(ctx, f.a.UserID, func(tx *sql.Tx) error {
			history, err := listMessages(ctx, tx, f.a.UserID, group.ConversationID, 0, 20, f.second.ID)
			for _, message := range history {
				for _, block := range message.Blocks {
					if block.BotMessage != nil {
						t.Error("私信泄露到其他群成员上下文")
					}
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("发起运行取消后停止子执行且不重新唤醒聊天", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		source := f.claim(t)
		f.start(t, source)
		result, sent := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "检查"})
		if result.IsError {
			t.Fatal(result)
		}
		if _, err := f.service.Cancel(ctx, f.a, source.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Complete(ctx, source, Outcome{Status: "canceled", ErrorCode: "CANCELED"}); err != nil {
			t.Fatal(err)
		}
		target := f.claim(t)
		worker := f.worker(t, callerFunc(func(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error) {
			t.Error("取消后仍执行子请求")
			return modelgateway.Result{}, nil
		}), testTool{})
		if run, err := worker.Process(ctx, target); err != nil || run.Status != "failed" {
			t.Fatal(run, err)
		}
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 0 {
			t.Fatal(count, err)
		}
		var status, code string
		if err := f.pool.QueryRow(`SELECT status,error_code FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, f.a.UserID, sent.OperationID).Scan(&status, &code); err != nil || status != "discarded" || code != "SOURCE_CANCELED" {
			t.Fatal(status, code, err)
		}
		if _, err := f.service.Claim(ctx, ids.New()); !errors.Is(err, ErrNoWork) {
			t.Fatal("取消后聊天被重启", err)
		}
	})
	t.Run("中间猫咪归档后向上级返回失败而非无限等待", func(t *testing.T) {
		f := newFixture(t, cluster)
		third := f.extraCat(t, "第三猫咪")
		f.send(t, f.a, f.first)
		root := f.claim(t)
		f.start(t, root)
		result, _ := f.privateCall(t, root, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "核对"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, root, "已安排")
		child := f.claim(t)
		f.start(t, child)
		result, _ = f.privateCall(t, child, "send_bot_message", botmessaging.Send{BotID: third.ID, Message: "读取资料"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, child, "等待资料")
		grandchild := f.claim(t)
		f.start(t, grandchild)
		f.finishPrivateRun(t, grandchild, "资料已核对")
		execute(t, f.pool, `UPDATE tombot.bots SET is_archived=true WHERE user_id=$1 AND id=$2`, f.a.UserID, f.second.ID)
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 0 {
			t.Fatal(count, err)
		}
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		resumed := f.claim(t)
		work := f.start(t, resumed)
		if resumed.BotID != f.first.ID || work.Messages[len(work.Messages)-1].Blocks[0].BotMessage.Status != "failed" {
			t.Fatal("未向上级返回失败")
		}
	})
	t.Run("接收权限撤回后不执行且给发送方明确失败结果", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		source := f.claim(t)
		f.start(t, source)
		result, _ := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "检查"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, source, "已安排")
		execute(t, f.pool, `UPDATE tombot.bots SET messaging_policy=jsonb_set(messaging_policy,'{accept_dm}','false') WHERE user_id=$1 AND id=$2`, f.a.UserID, f.second.ID)
		target := f.claim(t)
		worker := f.worker(t, callerFunc(func(context.Context, modelregistry.Configuration, []byte, modelgateway.Input, func(provider.Event) error) (modelgateway.Result, error) {
			t.Error("权限撤回后仍执行模型")
			return modelgateway.Result{}, nil
		}), testTool{})
		run, err := worker.Process(ctx, target)
		if err != nil || run.Status != "failed" {
			t.Fatal(run, err)
		}
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		resumed := f.claim(t)
		work := f.start(t, resumed)
		if work.Messages[len(work.Messages)-1].Blocks[0].BotMessage.Status != "failed" {
			t.Fatal("未向发送方说明失败")
		}
	})
	t.Run("跨租户无法读取私信正文或投递记录", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		source := f.claim(t)
		f.start(t, source)
		result, _ := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "仅限本用户的消息"})
		if result.IsError {
			t.Fatal(result)
		}
		if err := f.tenant.Tenant(ctx, f.b.UserID, func(tx *sql.Tx) error {
			var count int
			err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count)
			if count != 0 {
				t.Error("私信租户隔离失效")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
