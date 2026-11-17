package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/groupchat"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func (f fixture) group(t *testing.T) bots.Group {
	t.Helper()
	group, _, err := f.catalog.CreateGroup(context.Background(), f.a, bots.CreateGroupRequest{ClientRequestID: ids.New(), GroupSpec: bots.GroupSpec{Name: "讨论组", MemberIDs: []string{f.first.ID, f.second.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	return group
}
func TestGroupChatExecution(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("独立回合定向投递持久引用和租户隔离", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		request := SendRequest{ClientRequestID: ids.New(), Content: "@第一机器人 请核对", Mentions: []string{f.first.ID}}
		sent, err := f.service.Send(ctx, f.a, group.ConversationID, request)
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.service.Send(ctx, f.a, group.ConversationID, request)
		if err != nil || again.MessageID != sent.MessageID || again.IsCreated {
			t.Fatal(again, err)
		}
		lease := f.claim(t)
		if lease.BotID != f.first.ID {
			t.Fatal("唤醒错误成员")
		}
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if !strings.Contains(input.System, "shared group chat") || !strings.Contains(input.System, sent.MessageID) {
				t.Error("缺少群上下文")
			}
			return emitText(emit, "核对完成")
		}), testTool{})
		next, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "下一条", Mentions: []string{f.second.ID}})
		if err != nil || next.TurnID == sent.TurnID {
			t.Fatal("群聊输入被合并进旧回合", err)
		}
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "succeeded" {
			t.Fatal(run, err)
		}
		page, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 10)
		if err != nil || len(page) != 3 || page[0].ReplyTo == nil || page[0].ReplyTo.MessageID != sent.MessageID {
			t.Fatal(page, err)
		}
		reply, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "再解释一下", ReplyToID: page[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		replied, err := f.service.GetRun(ctx, f.a, reply.RunID)
		if err != nil || replied.BotID != f.first.ID {
			t.Fatal("回复未路由到原作者", err)
		}
		if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "越界", Mentions: []string{f.foreign.ID}}); !errors.Is(err, groupchat.ErrMention) {
			t.Fatal(err)
		}
		if _, err = f.service.Messages(ctx, f.b, group.ConversationID, 0, 10); !errors.Is(err, ErrNotFound) {
			t.Fatal("跨租户读取", err)
		}
		direct := f.send(t, f.a, f.first)
		if _, err = f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "越界引用", ReplyToID: direct.MessageID}); !errors.Is(err, ErrInvalidInput) {
			t.Fatal(err)
		}
	})
	t.Run("机器人转交及用户提及不产生循环", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		sent, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "检查并转交", Mentions: []string{f.first.ID}})
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			switch calls {
			case 1:
				return emitMemory(emit, "post_to_group", memoryArguments(t, map[string]any{"message": "请第二位继续", "mentions": []string{f.second.ID}, "reply_to_id": ""}))
			case 2:
				return emitText(emit, "已完成第一部分")
			default:
				if !strings.Contains(input.System, "Your bot ID: "+f.second.ID) {
					t.Error("成员身份不正确")
				}
				found := false
				for _, message := range input.Messages {
					for _, block := range message.Blocks {
						if strings.Contains(block.Text, "请第二位继续") {
							found = true
							if message.Role != "user" {
								t.Error("其他成员发言冒充自身助手角色")
							}
						}
					}
				}
				if !found {
					t.Error("没有转交原文")
				}
				return emitText(emit, "@用户 请确认结果。")
			}
		}), testTool{})
		first := f.claim(t)
		run, err := worker.Process(ctx, first)
		if err != nil || run.Status != "succeeded" {
			t.Fatal(run, err)
		}
		second := f.claim(t)
		if second.BotID != f.second.ID || second.TurnID != sent.TurnID {
			t.Fatal("转交目标或回合错误")
		}
		run, err = worker.Process(ctx, second)
		if err != nil || run.Status != "succeeded" {
			t.Fatal(run, err)
		}
		page, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || len(page) != 4 || len(page[0].Blocks[0].Mentions) != 1 || page[0].Blocks[0].Mentions[0].ID != "user" {
			t.Fatal(page, err)
		}
		lease, err := f.service.Claim(ctx, ids.New())
		if !errors.Is(err, ErrNoWork) {
			t.Fatal("@用户产生多余运行", lease, err)
		}
	})
}

func TestGroupChatBoundaries(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("广播且同键并发只受理一次", func(t *testing.T) {
		f := newFixture(t, cluster)
		group := f.group(t)
		request := SendRequest{ClientRequestID: ids.New(), Content: "所有人一起看看"}
		results := make(chan Submission, 2)
		failures := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				sent, err := f.service.Send(ctx, f.a, group.ConversationID, request)
				results <- sent
				failures <- err
			}()
		}
		first, second := <-results, <-results
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if first.MessageID != second.MessageID || first.IsCreated == second.IsCreated {
			t.Fatal("重复受理")
		}
		var notices int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.user_events WHERE user_id=$1 AND kind='group.run.queued'`, f.a.UserID).Scan(&notices); err != nil || notices != 2 {
			t.Fatal("群聊状态污染单聊事件", notices, err)
		}
		activity, err := f.service.Activity(ctx, f.a, group.ConversationID)
		if err != nil || len(activity) != 2 {
			t.Fatal("群活动没有包含全部成员", activity, err)
		}
		var count int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.runs WHERE turn_id=$1`, first.TurnID).Scan(&count); err != nil || count != 2 {
			t.Fatal(count, err)
		}
		execute(t, f.pool, `UPDATE tombot.user_limits SET queued_runs=2 WHERE user_id=$1`, f.a.UserID)
		if _, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "队列已满"}); !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
		messages, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || len(messages) != 1 {
			t.Fatal("失败受理残留消息", err)
		}
	})
	t.Run("循环转交有上限且留下通知", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		if _, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "循环检查", Mentions: []string{f.first.ID}}); err != nil {
			t.Fatal(err)
		}
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			target := f.first.Name
			if strings.Contains(input.System, "Your bot ID: "+f.first.ID) {
				target = f.second.Name
			}
			return emitText(emit, "@"+target+" 继续检查。")
		}), testTool{})
		for i := 0; i <= groupchat.MaxDepth; i++ {
			run, err := worker.Process(ctx, f.claim(t))
			if err != nil || run.Status != "succeeded" {
				t.Fatal(i, run, err)
			}
		}
		if _, err := f.service.Claim(ctx, ids.New()); !errors.Is(err, ErrNoWork) {
			t.Fatal("循环没有停止", err)
		}
		page, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || !strings.Contains(page[0].Blocks[0].Text, "上限") {
			t.Fatal("缺少上限通知", page, err)
		}
	})
	t.Run("成员被移出后不能继续发表或查看私人记忆", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		group := f.group(t)
		if _, err := f.service.Send(ctx, f.a, group.ConversationID, SendRequest{ClientRequestID: ids.New(), Content: "请核对", Mentions: []string{f.first.ID}}); err != nil {
			t.Fatal(err)
		}
		lease := f.claim(t)
		work, err := f.service.Start(ctx, lease)
		if err != nil || work.HasMemory {
			t.Fatal("群聊读取了私人记忆", err)
		}
		group.MemberIDs = []string{f.second.ID}
		if _, err = f.catalog.UpdateGroup(ctx, f.a, group.ID, bots.UpdateGroupRequest{ExpectedRevision: group.Revision, GroupSpec: group.GroupSpec}); err != nil {
			t.Fatal(err)
		}
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, _ modelgateway.Input, _ func(provider.Event) error) (modelgateway.Result, error) {
			t.Error("移出成员仍调用模型")
			return modelgateway.Result{}, ErrUnavailable
		}), testTool{})
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "failed" || run.ErrorCode != "RESOURCE_UNAVAILABLE" {
			t.Fatal(run, err)
		}
		page, err := f.service.Messages(ctx, f.a, group.ConversationID, 0, 20)
		if err != nil || !strings.Contains(page[0].Blocks[0].Text, "未能完成") {
			t.Fatal(page, err)
		}
	})
}
