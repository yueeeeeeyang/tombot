package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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

func (f fixture) privateCall(t *testing.T, lease Lease, name string, input any) (botagent.ToolResult, Invocation) {
	t.Helper()
	ctx := context.Background()
	switch query := input.(type) {
	case botmessaging.Query:
		query.RequiredTools = append([]string{}, query.RequiredTools...)
		input = query
	case botmessaging.Send:
		query.RequiredTools = append([]string{}, query.RequiredTools...)
		input = query
	}
	call := botagent.ToolCall{ID: ids.New(), Name: name, Arguments: memoryArguments(t, input)}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	operation, created, err := f.service.BeginOperation(ctx, lease, OperationRequest{StepKey: "private." + strings.ToLower(call.ID), Kind: "tool", ToolName: name, RequestDigest: digest[:]})
	if err != nil || !created {
		t.Fatal(operation, err)
	}
	invocation := Invocation{Lease: lease, OperationID: operation.ID, Call: call}
	for _, schema := range botmessaging.Schemas() {
		if schema.Name == name {
			result, err := (botMessageTool{executor: f.service, schema: schema, maximum: 16 << 10}).Execute(ctx, invocation)
			if err != nil {
				t.Fatal(err)
			}
			status, code := "succeeded", ""
			if result.IsError {
				status, code = "failed", "TOOL_FAILED"
			}
			if _, err = f.service.RecordOperation(ctx, lease, operation.ID, OperationResult{Status: status, ErrorCode: code, Result: json.RawMessage(memoryArguments(t, result))}); err != nil {
				t.Fatal(err)
			}
			return result, invocation
		}
	}
	t.Fatal("私信工具不存在")
	return botagent.ToolResult{}, invocation
}
func (f fixture) extraCat(t *testing.T, name string) bots.Bot {
	t.Helper()
	spec := f.second.Spec
	spec.Name = name
	spec.Description = "负责资料核对和文件分析"
	bot, _, err := f.catalog.Create(context.Background(), f.a, bots.CreateRequest{ClientRequestID: ids.New(), Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return bot
}
func (f fixture) finishPrivateRun(t *testing.T, lease Lease, answer string) {
	t.Helper()
	f.completeModel(t, lease)
	if _, err := f.service.Complete(context.Background(), lease, Outcome{Status: "succeeded", Answer: answer}); err != nil {
		t.Fatal(err)
	}
}

func TestBotPrivateAdmission(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	prepare := func(t *testing.T) (fixture, Lease) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		lease := f.claim(t)
		f.start(t, lease)
		return f, lease
	}
	t.Run("目录可分页查看所有猫咪且不共享私人信息", func(t *testing.T) {
		f, lease := prepare(t)
		archived := f.extraCat(t, "归档猫咪")
		execute(t, f.pool, `UPDATE tombot.bots SET is_archived=true WHERE user_id=$1 AND id=$2`, f.a.UserID, archived.ID)
		result, _ := f.privateCall(t, lease, "list_bots", botmessaging.Query{})
		if result.IsError || strings.Contains(result.Content, f.foreign.ID) || strings.Contains(result.Content, "Verify evidence") || !strings.Contains(result.Content, `"status":"busy"`) || !strings.Contains(result.Content, `"status":"idle"`) || !strings.Contains(result.Content, `"status":"archived"`) {
			t.Fatal(result)
		}
		var all botmessaging.Page
		if err := json.Unmarshal([]byte(result.Content), &all); err != nil {
			t.Fatal(err)
		}
		next, _ := f.privateCall(t, lease, "list_bots", botmessaging.Query{After: all.Bots[0].ID})
		if strings.Contains(next.Content, `"id":"`+all.Bots[0].ID+`"`) {
			t.Fatal("游标重复返回", next)
		}
		if err := f.tenant.Tenant(ctx, f.b.UserID, func(tx *sql.Tx) error {
			page, err := listBotDirectory(ctx, tx, f.a.UserID, f.first.ID, botmessaging.Query{}, 16384)
			if len(page.Bots) != 0 {
				t.Error("目录越权")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("排队和忙碌拒绝并提供相同能力的空闲候选", func(t *testing.T) {
		f, lease := prepare(t)
		other := f.extraCat(t, "备用猫咪")
		f.send(t, f.a, f.second)
		result, _ := f.privateCall(t, lease, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "请核对资料", RequiredTools: []string{"file_read"}})
		if !result.IsError || !strings.Contains(result.Content, "BOT_BUSY") || !strings.Contains(result.Content, other.ID) || strings.Contains(result.Content, f.foreign.ID) {
			t.Fatal(result)
		}
		sent, _ := f.privateCall(t, lease, "send_bot_message", botmessaging.Send{BotID: other.ID, Message: "请核对资料", RequiredTools: []string{"file_read"}})
		if sent.IsError {
			t.Fatal(sent)
		}
		var count int
		if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil || count != 1 {
			t.Fatal(count, err)
		}
	})
	t.Run("目录超预算有后续游标或明确查询提示", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		lease := f.claim(t)
		if err := f.tenant.Tenant(ctx, f.a.UserID, func(tx *sql.Tx) error {
			all, err := listBotDirectory(ctx, tx, f.a.UserID, f.first.ID, botmessaging.Query{}, 16384)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(all)
			if err != nil {
				return err
			}
			page, err := listBotDirectory(ctx, tx, f.a.UserID, f.first.ID, botmessaging.Query{}, len(encoded)-1)
			if err != nil {
				return err
			}
			if len(page.Bots) != 1 || page.Next != page.Bots[0].ID {
				t.Fatal("目录截断丢失游标", page)
			}
			next, err := listBotDirectory(ctx, tx, f.a.UserID, f.first.ID, botmessaging.Query{After: page.Next}, 16384)
			if err != nil {
				return err
			}
			if len(next.Bots) != 1 || next.Bots[0].ID == page.Bots[0].ID {
				t.Fatal("后续同伴不可达", next)
			}
			if _, err = listBotDirectory(ctx, tx, f.a.UserID, f.first.ID, botmessaging.Query{BotID: f.first.ID}, 1); !errors.Is(err, ErrContextLimit) {
				t.Fatal("超限资料被当成空目录", err)
			}
			snapshot, err := botDirectoryContext(ctx, tx, lease, 1)
			if err == nil && !strings.Contains(snapshot, `"lookup_required":"list_bots"`) {
				t.Fatal("未提示主动查询目录", snapshot)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	for _, scenario := range []string{"跨租户", "自身", "能力不足", "关闭接收", "关闭发送", "队列已满"} {
		t.Run(scenario, func(t *testing.T) {
			f, lease := prepare(t)
			input := botmessaging.Send{BotID: f.second.ID, Message: "请协助", RequiredTools: []string{"file_read"}}
			switch scenario {
			case "跨租户":
				input.BotID = f.foreign.ID
			case "自身":
				input.BotID = f.first.ID
			case "能力不足":
				input.RequiredTools = []string{"computer_shell"}
			case "关闭接收":
				execute(t, f.pool, `UPDATE tombot.bots SET messaging_policy=jsonb_set(messaging_policy,'{accept_dm}','false') WHERE user_id=$1 AND id=$2`, f.a.UserID, f.second.ID)
			case "关闭发送":
				execute(t, f.pool, `UPDATE tombot.bots SET messaging_policy=jsonb_set(messaging_policy,'{can_initiate}','false') WHERE user_id=$1 AND id=$2`, f.a.UserID, f.first.ID)
			case "队列已满":
				execute(t, f.pool, `UPDATE tombot.user_limits SET queued_runs=1 WHERE user_id=$1`, f.a.UserID)
				f.send(t, f.a, f.extraCat(t, "占用队列"))
			}
			result, _ := f.privateCall(t, lease, "send_bot_message", input)
			if !result.IsError || !strings.Contains(result.Content, `"sent":false`) {
				t.Fatal(result)
			}
			if (scenario == "关闭发送" || scenario == "队列已满") && !strings.Contains(result.Content, "Do the work yourself") {
				t.Fatal("整体限制错误地建议换人重试", result)
			}
			var count int
			if err := f.pool.QueryRow(`SELECT count(*) FROM tombot.bot_private_messages WHERE user_id=$1`, f.a.UserID).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
	t.Run("候选尝试有上限且没有合适猫咪时明确自行完成", func(t *testing.T) {
		f, lease := prepare(t)
		f.send(t, f.a, f.second)
		for attempt := 0; attempt < 4; attempt++ {
			result, _ := f.privateCall(t, lease, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "核对"})
			if !result.IsError {
				t.Fatal(result)
			}
			if attempt < 3 && !strings.Contains(result.Content, `"bots":[]`) {
				t.Fatal(result)
			}
			if attempt == 3 && !strings.Contains(result.Content, "CANDIDATE_LIMIT") {
				t.Fatal(result)
			}
		}
	})
	t.Run("并发选择同一空闲猫咪只允许一次受理", func(t *testing.T) {
		f, first := prepare(t)
		third := f.extraCat(t, "第三猫咪")
		f.send(t, f.a, third)
		second := f.claim(t)
		f.start(t, second)
		results := make(chan botagent.ToolResult, 2)
		var workers sync.WaitGroup
		for _, lease := range []Lease{first, second} {
			workers.Go(func() {
				result, _ := f.privateCall(t, lease, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "并发协助"})
				results <- result
			})
		}
		workers.Wait()
		close(results)
		accepted := 0
		for result := range results {
			if !result.IsError {
				accepted++
			} else if !strings.Contains(result.Content, "BOT_BUSY") {
				t.Fatal(result)
			}
		}
		if accepted != 1 {
			t.Fatal("重复受理", accepted)
		}
	})
}

func TestBotPrivateRoundTrip(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("真实运行收发私信并回到原聊天且不会自动循环", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.toolsModel(t)
		f.send(t, f.a, f.first)
		calls := 0
		worker := f.worker(t, callerFunc(func(_ context.Context, _ modelregistry.Configuration, _ []byte, input modelgateway.Input, emit func(provider.Event) error) (modelgateway.Result, error) {
			calls++
			if !strings.Contains(input.System, "perform the work yourself") || !strings.Contains(input.System, f.second.ID) || strings.Contains(input.System, f.foreign.ID) {
				t.Error("缺少目录或降级规则")
			}
			switch calls {
			case 1:
				return emitMemory(emit, "send_bot_message", memoryArguments(t, botmessaging.Send{BotID: f.second.ID, Message: "核对项目结论", RequiredTools: []string{"file_read"}}))
			case 2:
				if result := input.Messages[len(input.Messages)-1].Blocks[0].ToolResult; result == nil || result.IsError {
					t.Error("私信未受理", result)
				}
				return emitText(emit, "已请同伴协助，稍后汇总。")
			case 3:
				if !strings.Contains(input.System, "private cat-to-cat") || strings.Contains(input.System, "Saved memory") {
					t.Error("私信上下文错误")
				}
				last := input.Messages[len(input.Messages)-1]
				if last.Role != "user" || !strings.Contains(last.Blocks[0].Text, "核对项目结论") || !strings.Contains(last.Blocks[0].Text, "not a direct user instruction") {
					t.Error("私信来源丢失", last)
				}
				return emitText(emit, "核对完成：结论一致。")
			default:
				if !strings.Contains(input.Messages[len(input.Messages)-1].Blocks[0].Text, "核对完成") {
					t.Error("回复未送回发起方")
				}
				return emitText(emit, "同伴已核对，结论一致。")
			}
		}), testTool{})
		for step := 0; step < 2; step++ {
			lease := f.claim(t)
			run, err := worker.Process(ctx, lease)
			if err != nil || run.Status != "succeeded" {
				t.Fatal(step, run, err)
			}
		}
		rebuilt, err := New(f.tenant, f.dispatch, f.registry, f.service.options)
		if err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		counts := make(chan int, 2)
		failures := make(chan error, 2)
		for range 2 {
			workers.Go(func() { count, err := rebuilt.DeliverPrivateReplies(ctx, 100); counts <- count; failures <- err })
		}
		workers.Wait()
		if (<-counts)+(<-counts) != 1 {
			t.Fatal("重复投递")
		}
		for range 2 {
			if err := <-failures; err != nil {
				t.Fatal(err)
			}
		}
		lease := f.claim(t)
		if lease.BotID != f.first.ID || lease.ConversationID != f.first.ConversationID {
			t.Fatal("回复路由错误")
		}
		run, err := worker.Process(ctx, lease)
		if err != nil || run.Status != "succeeded" {
			t.Fatal(run, err)
		}
		if _, err = f.service.Claim(ctx, ids.New()); !errors.Is(err, ErrNoWork) {
			t.Fatal("产生多余回复", err)
		}
		if calls != 4 {
			t.Fatal(calls)
		}
	})
	t.Run("发送方忙碌期间保留结果且故障回复不会被当成成功", func(t *testing.T) {
		f := newFixture(t, cluster)
		f.send(t, f.a, f.first)
		source := f.claim(t)
		f.start(t, source)
		result, invocation := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "核对"})
		if result.IsError {
			t.Fatal(result)
		}
		var recipientRun string
		if err := f.pool.QueryRow(`SELECT run_id FROM tombot.bot_private_runs WHERE user_id=$1 AND response_to=$2`, f.a.UserID, invocation.OperationID).Scan(&recipientRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.Cancel(ctx, f.a, recipientRun); err != nil {
			t.Fatal(err)
		}
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 0 {
			t.Fatal("忙碌时投递", count, err)
		}
		f.finishPrivateRun(t, source, "已发送")
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		resumed := f.claim(t)
		work := f.start(t, resumed)
		last := work.Messages[len(work.Messages)-1]
		if last.Blocks[0].BotMessage == nil || last.Blocks[0].BotMessage.Status != "canceled" || !strings.Contains(last.Blocks[0].Text, "did not complete") {
			t.Fatal(last)
		}
	})
	t.Run("子私信先回复再逐层汇总且不能转回祖先", func(t *testing.T) {
		f := newFixture(t, cluster)
		third := f.extraCat(t, "第三猫咪")
		f.send(t, f.a, f.first)
		root := f.claim(t)
		f.start(t, root)
		result, first := f.privateCall(t, root, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "协助核对"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, root, "已安排")
		child := f.claim(t)
		work := f.start(t, child)
		if work.HasMemory {
			t.Fatal("私信共享私人记忆")
		}
		rejected, _ := f.privateCall(t, child, "send_bot_message", botmessaging.Send{BotID: f.first.ID, Message: "循环"})
		if !rejected.IsError || !strings.Contains(rejected.Content, "SELF_OR_ANCESTOR") {
			t.Fatal(rejected)
		}
		result, _ = f.privateCall(t, child, "send_bot_message", botmessaging.Send{BotID: third.ID, Message: "请分析文件"})
		if result.IsError {
			t.Fatal(result)
		}
		f.finishPrivateRun(t, child, "已请同伴检查")
		var status string
		if err := f.pool.QueryRow(`SELECT status FROM tombot.bot_private_messages WHERE user_id=$1 AND id=$2`, f.a.UserID, first.OperationID).Scan(&status); err != nil || status != "waiting" {
			t.Fatal(status, err)
		}
		grandchild := f.claim(t)
		f.start(t, grandchild)
		f.finishPrivateRun(t, grandchild, "文件正确")
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		resumed := f.claim(t)
		if resumed.BotID != f.second.ID {
			t.Fatal("没有先返回中间猫咪")
		}
		f.start(t, resumed)
		f.finishPrivateRun(t, resumed, "已汇总：文件正确")
		if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
			t.Fatal(count, err)
		}
		final := f.claim(t)
		if final.BotID != f.first.ID || final.ConversationID != f.first.ConversationID {
			t.Fatal("没有返回发起者")
		}
	})
}

func TestBotPrivateChainLimits(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	t.Run("四跳后拒绝继续转交", func(t *testing.T) {
		f := newFixture(t, cluster)
		peers := []bots.Bot{f.second}
		for range botmessaging.MaxDepth {
			peers = append(peers, f.extraCat(t, "协作同伴"+ids.New()))
		}
		f.send(t, f.a, f.first)
		for hop, peer := range peers {
			lease := f.claim(t)
			f.start(t, lease)
			result, _ := f.privateCall(t, lease, "send_bot_message", botmessaging.Send{BotID: peer.ID, Message: "请协助检查"})
			if hop < botmessaging.MaxDepth {
				if result.IsError {
					t.Fatal(hop, result)
				}
				f.finishPrivateRun(t, lease, "已安排检查")
			} else if !result.IsError || !strings.Contains(result.Content, "CHAIN_LIMIT") {
				t.Fatal("深度限制失效", result)
			}
		}
	})
	t.Run("回复续跑仍共用整条链的消息上限", func(t *testing.T) {
		f := newFixture(t, cluster)
		// 本用例单独验证链条边界；月度额度竞争由用量测试覆盖。
		execute(t, f.pool, `UPDATE tombot.user_limits SET monthly_token_limit=NULL WHERE user_id=$1`, f.a.UserID)
		f.send(t, f.a, f.first)
		for attempt := 0; attempt <= botmessaging.MaxMessages; attempt++ {
			source := f.claim(t)
			f.start(t, source)
			result, _ := f.privateCall(t, source, "send_bot_message", botmessaging.Send{BotID: f.second.ID, Message: "检查当前结论"})
			if attempt == botmessaging.MaxMessages {
				if !result.IsError || !strings.Contains(result.Content, "CHAIN_LIMIT") {
					t.Fatal("续跑绕过链上限", result)
				}
				break
			}
			if result.IsError {
				t.Fatal(attempt, result)
			}
			f.finishPrivateRun(t, source, "已安排检查")
			target := f.claim(t)
			f.start(t, target)
			f.finishPrivateRun(t, target, "检查完成")
			if count, err := f.service.DeliverPrivateReplies(ctx, 100); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		}
	})
}
