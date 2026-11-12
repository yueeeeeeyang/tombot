package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/service"
)

type messageTestTools struct {
	db   *sql.DB
	bots *service.Bots
}

func (messageTestTools) Schemas(domain.Bot) []provider.ToolSchema { return nil }
func (e messageTestTools) Execute(ctx context.Context, invocation domain.ToolInvocation, use domain.ToolUseBlock) (domain.ToolResultBlock, error) {
	result := domain.ToolResultBlock{ToolCallID: use.ID}
	encoded, err := json.Marshal(use.Input)
	if err != nil {
		return result, err
	}
	var input domain.BotMessageRequest
	if err = json.Unmarshal(encoded, &input); err != nil {
		return result, err
	}
	input.Delegation = use.Name == "delegate_task"
	target, err := e.bots.Get(ctx, input.BotID)
	if err != nil {
		return result, err
	}
	input.TargetBinding, err = service.MessagingBinding(target)
	if err != nil {
		return result, err
	}
	if _, err = e.db.ExecContext(ctx, `INSERT INTO tool_calls(id,run_id,step,tool_name,input_json,status,created_at) VALUES(?,?,?,?,?,'running',0)`, use.ID, invocation.Run.ID, invocation.Step, use.Name, string(encoded)); err != nil {
		return result, err
	}
	reply, err := invocation.Messenger.SendBotMessage(ctx, invocation.Run, use.ID, input)
	var public *domain.Error
	if errors.As(err, &public) {
		result.IsError = true
		result.Content = public.Error()
		err = nil
	} else if err == nil {
		encoded, err = json.Marshal(reply)
		result.Content = string(encoded)
	}
	if err != nil {
		return result, err
	}
	status := "succeeded"
	if result.IsError {
		status = "failed"
	}
	_, err = e.db.ExecContext(ctx, "UPDATE tool_calls SET status=? WHERE id=?", status, use.ID)
	return result, err
}

func enableMessages(t *testing.T, r *Runtime, bot domain.Bot) domain.Bot {
	t.Helper()
	policy := bot.MessagingPolicy
	policy.CanInitiate = true
	updated, err := r.deps.Bots.Update(context.Background(), bot.ID, service.BotPatch{MessagingPolicy: service.Field[domain.MessagingPolicy]{Set: true, Value: policy}}, "user")
	if err != nil {
		t.Fatal(err)
	}
	r.deps.Tools = messageTestTools{r.deps.DB, r.deps.Bots}
	return updated
}
func emitMessageCall(emit func(provider.Event) error, target string, wait bool) error {
	encoded, err := json.Marshal(domain.BotMessageRequest{BotID: target, Message: "请核验公开资料", ReplyRequired: wait})
	if err != nil {
		return err
	}
	if err = emit(provider.Event{Kind: "tool_use_start", ToolCallID: "send", ToolName: "message_bot"}); err != nil {
		return err
	}
	return emit(provider.Event{Kind: "tool_use_end", ToolCallID: "send", InputDelta: string(encoded)})
}
func lastToolResult(req provider.Request) *domain.ToolResultBlock {
	if len(req.Messages) > 0 {
		for _, block := range req.Messages[len(req.Messages)-1].Blocks {
			if block.ToolResult != nil {
				return block.ToolResult
			}
		}
	}
	return nil
}

func TestBotMessagesReusePairsLimitRateAndIntervene(t *testing.T) {
	target := ""
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.Contains(req.System, "This is a one-to-one bot conversation") {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "核验完成"})
		}
		if result := lastToolResult(req); result != nil {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: result.Content})
		}
		return provider.Usage{}, emitMessageCall(emit, target, true)
	}), DefaultLimits())
	bot = enableMessages(t, r, bot)
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target = peer.ID
	for range 7 {
		sent := submit(t, r, bot)
		if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
			t.Fatal(run)
		}
	}
	var count int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL").Scan(&count); err != nil || count != 6 {
		t.Fatal("频率限制错误", count, err)
	}
	var conversationID string
	if err = r.deps.DB.QueryRow("SELECT id FROM conversations WHERE kind='bot_dm'").Scan(&conversationID); err != nil {
		t.Fatal(err)
	}
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM conversations WHERE kind='bot_dm'").Scan(&count); err != nil || count != 1 {
		t.Fatal("未复用成员对话", count, err)
	}
	if err = r.deps.DB.QueryRow("SELECT COUNT(DISTINCT c.bot_id) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.kind='direct' AND m.blocks_json LIKE '%' || ? || '%'", conversationID).Scan(&count); err != nil || count != 2 {
		t.Fatal("双方单聊都要留下私信提示", count, err)
	}
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE blocks_json LIKE '%BOT_MESSAGE_RATE_LIMIT%'").Scan(&count); err != nil || count < 1 {
		t.Fatal("未提示限流", count, err)
	}
	_, err = r.Submit(context.Background(), conversationID, SendInput{Content: "核验", RecipientBotID: "outsider"}, "user")
	requireCode(t, err, "INVALID_DM_RECIPIENT")
	for _, id := range []string{bot.ID, peer.ID} {
		sent, err := r.Submit(context.Background(), conversationID, SendInput{Content: "请补充依据", RecipientBotID: id}, "user")
		if err != nil {
			t.Fatal(err)
		}
		run := waitRun(t, r, sent.RunID)
		if run.Status != "succeeded" || run.BotID != id || run.ParentRunID != nil {
			t.Fatal("介入收件人错误", run)
		}
	}
}

func TestBotMessageHopBoundaryAndAncestorLoop(t *testing.T) {
	for _, loop := range []bool{false, true} {
		t.Run(fmt.Sprint(loop), func(t *testing.T) {
			targets := map[string]string{}
			r, first := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if result := lastToolResult(req); result != nil {
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: result.Content})
				}
				return provider.Usage{}, emitMessageCall(emit, targets[strings.Split(req.System, "\n")[1]], true)
			}), DefaultLimits())
			first = enableMessages(t, r, first)
			bots := []domain.Bot{first}
			for index := 1; index < 6; index++ {
				bot, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer", Name: fmt.Sprintf("成员%d", index)}, "user")
				if err != nil {
					t.Fatal(err)
				}
				bots = append(bots, enableMessages(t, r, bot))
			}
			for index := 0; index < 5; index++ {
				targets["Name: "+bots[index].Name] = bots[index+1].ID
			}
			code := "BOT_MESSAGE_HOPS"
			expected := 4
			if loop {
				targets["Name: "+bots[2].Name] = first.ID
				code = "BOT_MESSAGE_LOOP"
				expected = 2
			}
			sent := submit(t, r, first)
			if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
				t.Fatal(run)
			}
			var count int
			if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL").Scan(&count); err != nil || count != expected {
				t.Fatal(count, err)
			}
			if err := r.deps.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE blocks_json LIKE ?", "%"+code+"%").Scan(&count); err != nil || count == 0 {
				t.Fatal("缺少边界错误", code, count, err)
			}
		})
	}
}

func TestBotMessageFailureAndParentCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelParent), func(t *testing.T) {
			target := ""
			started := make(chan struct{})
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "This is a one-to-one bot conversation") {
					close(started)
					if cancelParent {
						<-ctx.Done()
						return provider.Usage{InputTokens: 3}, ctx.Err()
					}
					return provider.Usage{InputTokens: 3}, errors.New("目标执行失败")
				}
				if result := lastToolResult(req); result != nil {
					if !result.IsError || !strings.Contains(result.Content, "BOT_NO_REPLY") {
						return provider.Usage{}, errors.New("未明确报告目标失败")
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "目标未回复"})
				}
				return provider.Usage{}, emitMessageCall(emit, target, true)
			}), DefaultLimits())
			bot = enableMessages(t, r, bot)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			sent := submit(t, r, bot)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("目标未启动")
			}
			status := "succeeded"
			if cancelParent {
				status = "canceled"
				if err = r.Cancel(context.Background(), sent.TurnID, "user"); err != nil {
					t.Fatal(err)
				}
			}
			run := waitRun(t, r, sent.RunID)
			if run.Status != status || run.CostMicro != 3 {
				t.Fatal("失败或取消未结算", run)
			}
			var childStatus string
			if err = r.deps.DB.QueryRow("SELECT status FROM runs WHERE parent_run_id=?", run.ID).Scan(&childStatus); err != nil || childStatus == "running" {
				t.Fatal(childStatus, err)
			}
		})
	}
}

func TestBotMessageSingleSlotReplyPrivacyAndInheritedCost(t *testing.T) {
	target := ""
	limits := DefaultLimits()
	limits.Concurrent = 1
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		usage := provider.Usage{InputTokens: 5}
		if strings.Contains(req.System, "This is a one-to-one bot conversation") {
			encoded, err := json.Marshal(req.Messages)
			if err != nil {
				return usage, err
			}
			if strings.Contains(string(encoded), "不会转发的思考") || !strings.Contains(string(encoded), "请核验公开资料") {
				return usage, errors.New("私信上下文未隔离")
			}
			return usage, emit(provider.Event{Kind: "text_delta", Text: "目标成员已核验"})
		}
		if result := lastToolResult(req); result != nil {
			if result.IsError || !strings.Contains(result.Content, "目标成员已核验") {
				return usage, fmt.Errorf("没有收到目标回复：%+v", result)
			}
			return usage, emit(provider.Event{Kind: "text_delta", Text: "已收到同事结论"})
		}
		if err := emit(provider.Event{Kind: "thinking_delta", Text: "不会转发的思考"}); err != nil {
			return usage, err
		}
		return usage, emitMessageCall(emit, target, true)
	}), limits)
	bot = enableMessages(t, r, bot)
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target = peer.ID
	sent := submit(t, r, bot)
	run := waitRun(t, r, sent.RunID)
	if run.Status != "succeeded" || run.CostMicro != 15 {
		t.Fatal("单槽同步私信死锁或费用错误", run)
	}
	var childID string
	if err = r.deps.DB.QueryRow("SELECT id FROM runs WHERE parent_run_id=?", run.ID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	child, err := r.Run(context.Background(), childID)
	if err != nil || child.Status != "succeeded" || child.CostMicro != 5 || child.HopDepth != 1 || child.ParentRunID == nil || *child.ParentRunID != run.ID {
		t.Fatal("私信子执行归属错误", child, err)
	}
}

func TestAsyncBotMessageReturnsBeforeReplyButDrainsBeforeFinish(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	target := ""
	limits := DefaultLimits()
	limits.Concurrent = 1
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		usage := provider.Usage{InputTokens: 2}
		if strings.Contains(req.System, "This is a one-to-one bot conversation") {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return usage, ctx.Err()
			}
			return usage, emit(provider.Event{Kind: "text_delta", Text: "异步完成"})
		}
		if result := lastToolResult(req); result != nil {
			if result.IsError || !strings.Contains(result.Content, `"delivered":true`) || strings.Contains(result.Content, "异步完成") {
				return usage, errors.New("异步调用没有立即返回投递结果")
			}
			return usage, emit(provider.Event{Kind: "text_delta", Text: "已投递"})
		}
		return usage, emitMessageCall(emit, target, false)
	}), limits)
	bot = enableMessages(t, r, bot)
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target = peer.ID
	sent := submit(t, r, bot)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("异步执行没有取得父任务让出的槽")
	}
	parent, err := r.Run(context.Background(), sent.RunID)
	if err != nil || parent.Status != "running" {
		t.Fatal("子任务未结算便完成父任务", parent, err)
	}
	close(release)
	parent = waitRun(t, r, sent.RunID)
	if parent.Status != "succeeded" || parent.CostMicro != 6 {
		t.Fatal("异步费用未统一收尾", parent)
	}
}

func TestConcurrentCrossBotMessagesCannotDeadlock(t *testing.T) {
	var mu sync.Mutex
	targets := map[string]string{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	r, first := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.Contains(req.System, "This is a one-to-one bot conversation") {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已接收"})
		}
		if result := lastToolResult(req); result != nil {
			return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "已处理投递结果"})
		}
		identity := strings.Split(req.System, "\n")[1]
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
		mu.Lock()
		target := targets[identity]
		mu.Unlock()
		return provider.Usage{}, emitMessageCall(emit, target, false)
	}), DefaultLimits())
	first = enableMessages(t, r, first)
	second, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	second = enableMessages(t, r, second)
	targets["Name: "+first.Name] = second.ID
	targets["Name: "+second.Name] = first.ID
	a := submit(t, r, first)
	b := submit(t, r, second)
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("调用方未并行开始")
		}
	}
	close(release)
	if waitRun(t, r, a.RunID).Status != "succeeded" || waitRun(t, r, b.RunID).Status != "succeeded" {
		t.Fatal("交叉调用没有完成")
	}
	var count int
	if err = r.deps.DB.QueryRow("SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatal("交叉依赖未被限制", count, err)
	}
}

func TestDelegatedReplyRequiresStructuredSharedArtifacts(t *testing.T) {
	for _, test := range []struct{ body, code string }{
		{`{"summary":"核验完成","artifacts":["shared/result.md"]}`, ""},
		{`没有结构化结果`, "BOT_REPLY_INVALID"},
		{`{"summary":"核验完成"}`, "BOT_REPLY_INVALID"},
		{`{"summary":"核验完成","artifacts":[]} trailing`, "BOT_REPLY_INVALID"},
		{`{"summary":"核验完成","artifacts":["notes/private.md"]}`, "BOT_ARTIFACT_INVALID"},
	} {
		t.Run(test.code+test.body, func(t *testing.T) {
			target := ""
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "This is a delegated task.") {
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: test.body})
				}
				if result := lastToolResult(req); result != nil {
					if (test.code == "" && result.IsError) || (test.code != "" && (!result.IsError || !strings.Contains(result.Content, test.code))) {
						return provider.Usage{}, fmt.Errorf("委派结果校验错误：%+v", result)
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "委派结果已处理"})
				}
				encoded, err := json.Marshal(domain.BotMessageRequest{BotID: target, Message: "生成共享报告"})
				if err != nil {
					return provider.Usage{}, err
				}
				if err = emit(provider.Event{Kind: "tool_use_start", ToolCallID: "delegate", ToolName: "delegate_task"}); err != nil {
					return provider.Usage{}, err
				}
				return provider.Usage{}, emit(provider.Event{Kind: "tool_use_end", ToolCallID: "delegate", InputDelta: string(encoded)})
			}), DefaultLimits())
			bot = enableMessages(t, r, bot)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			sent := submit(t, r, bot)
			if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
				t.Fatal(run)
			}
		})
	}
}

func TestBotMessageInheritedBudgetStopsParentContinuation(t *testing.T) {
	limits := DefaultLimits()
	limits.BudgetMicro = 10
	target := ""
	r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
		if strings.Contains(req.System, "This is a one-to-one bot conversation") {
			return provider.Usage{InputTokens: 8}, emit(provider.Event{Kind: "text_delta", Text: "费用已结算"})
		}
		if lastToolResult(req) != nil {
			return provider.Usage{}, errors.New("继承预算已耗尽仍继续调用模型")
		}
		return provider.Usage{InputTokens: 2}, emitMessageCall(emit, target, true)
	}), limits)
	bot = enableMessages(t, r, bot)
	peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	target = peer.ID
	sent := submit(t, r, bot)
	run := waitRun(t, r, sent.RunID)
	if run.Status != "failed" || run.CostMicro != 10 || (run.ErrorCode == nil || (*run.ErrorCode != "RUN_BUDGET_EXCEEDED" && *run.ErrorCode != "TURN_BUDGET_EXCEEDED")) {
		t.Fatal(run)
	}
	var budget int64
	if err = r.deps.DB.QueryRow("SELECT budget_micro FROM runs WHERE parent_run_id=?", run.ID).Scan(&budget); err != nil || budget != 8 {
		t.Fatal("子任务未继承剩余预算", budget, err)
	}
}

func TestAsyncBotFailureLeavesVisibleNoticeAndTimeoutReturnsNoReply(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprint(synchronous), func(t *testing.T) {
			target := ""
			limits := DefaultLimits()
			limits.StepTime = 300 * time.Millisecond
			r, bot := runtimeFixture(t, script(func(ctx context.Context, req provider.Request, emit func(provider.Event) error) (provider.Usage, error) {
				if strings.Contains(req.System, "This is a one-to-one bot conversation") {
					if synchronous {
						<-ctx.Done()
						return provider.Usage{}, ctx.Err()
					}
					return provider.Usage{}, errors.New("异步目标失败")
				}
				if result := lastToolResult(req); result != nil {
					if synchronous && (!result.IsError || !strings.Contains(result.Content, "BOT_NO_REPLY")) {
						return provider.Usage{}, errors.New("目标超时没有明确错误")
					}
					return provider.Usage{}, emit(provider.Event{Kind: "text_delta", Text: "处理完成"})
				}
				return provider.Usage{}, emitMessageCall(emit, target, synchronous)
			}), limits)
			bot = enableMessages(t, r, bot)
			peer, err := r.deps.Bots.Create(context.Background(), service.BotInput{Template: "writer"}, "user")
			if err != nil {
				t.Fatal(err)
			}
			target = peer.ID
			sent := submit(t, r, bot)
			if run := waitRun(t, r, sent.RunID); run.Status != "succeeded" {
				t.Fatal(run)
			}
			if !synchronous {
				messages, err := service.NewConversations(r.deps.DB, r.deps.Events).Messages(context.Background(), bot.ConversationID, 0, 50)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, message := range messages {
					for _, block := range message.Blocks {
						if block.Kind == "note" && strings.Contains(block.Text, "未能完成") {
							found = true
						}
					}
				}
				if !found {
					t.Fatal("异步失败被静默忽略")
				}
			}
		})
	}
}
