package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io.yueyang/tom/internal/service"
	"strings"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/store"
	ledger "io.yueyang/tom/internal/usage"
)

func (r *Runtime) loop(ctx context.Context, j *job) error {
	defer func() {
		if j.summaryDone == nil {
			return
		}
		if err := <-j.summaryDone; err != nil {
			r.deps.Logger.Warn("会话摘要更新失败，保留原摘要与转录", "run_id", j.run.ID, "conversation_id", j.run.ConversationID)
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, noticeErr := r.deps.Events.Publish(cleanup, "system.notice", j.run.ConversationID, map[string]string{"level": "warning", "text": "较早消息的摘要未更新；原始消息已保留。"}); noticeErr != nil {
				r.deps.Logger.Error("摘要失败通知保存失败", "run_id", j.run.ID)
			}
		}
	}()
	for step := 0; step < r.limits.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.deps.DB.QueryRowContext(ctx, "SELECT cost_micro FROM runs WHERE id=?", j.run.ID).Scan(&j.run.CostMicro); err != nil {
			return err
		}
		if err := ledger.CheckBudget(ctx, r.deps.DB, j.run.ID); err != nil {
			return err
		}
		req, err := r.context(ctx, j)
		if err != nil {
			return err
		}
		message, usage, streamErr := r.stream(ctx, j, req)
		// 上游中断也可能产生费用；在取消后的独立短事务中保留已经确认的用量。
		persist, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		total, usageErr := ledger.Record(persist, r.deps.DB, j.run, j.model, usage)
		if usageErr == nil {
			j.run.CostMicro = total
			r.deps.Events.Notify()
		}
		cancel()
		if streamErr != nil || usageErr != nil {
			return errors.Join(streamErr, usageErr)
		}
		uses := []domain.ToolUseBlock{}
		for _, block := range message.Blocks {
			if block.Kind == "tool_use" && block.ToolUse != nil {
				uses = append(uses, *block.ToolUse)
			}
		}
		if len(uses) == 0 {
			return nil
		}
		if j.mode == "summary" {
			return domain.NewError(502, "GROUP_SUMMARY_TOOLS", "The summary must not invoke tools.")
		}
		var toolErr error
		toolErr = ledger.CheckBudget(ctx, r.deps.DB, j.run.ID)
		results := []domain.MessageBlock{}
		for _, use := range uses {
			result := domain.ToolResultBlock{ToolCallID: use.ID, IsError: true, Content: "TOOL_DENIED: This tool is not available to this bot."}
			if ctx.Err() != nil {
				toolErr = ctx.Err()
			}
			if toolErr != nil {
				result.Content = "TOOL_CANCELED: The run stopped before this tool completed."
			} else if r.deps.Tools != nil {
				result, err = r.deps.Tools.Execute(ctx, domain.ToolInvocation{Run: j.run, Bot: j.bot, Step: step, Messenger: runMessenger{r, j}, UserWaiter: runUserWaiter{r, j}}, use)
				if err != nil {
					toolErr = err
					result = domain.ToolResultBlock{ToolCallID: use.ID, IsError: true, Content: "TOOL_FAILED: The tool stopped before completion."}
				}
				if result.ToolCallID != use.ID {
					toolErr = domain.NewError(500, "TOOL_RESULT_INVALID", "The tool result does not match its call.")
					result = domain.ToolResultBlock{ToolCallID: use.ID, IsError: true, Content: "TOOL_FAILED: Invalid tool result."}
				}
			}
			results = append(results, domain.MessageBlock{Kind: "tool_result", ToolResult: &result})
		}
		resultMessage := domain.Message{ID: ids.New(), ConversationID: j.run.ConversationID, TurnID: &j.run.TurnID, RunID: &j.run.ID, SenderKind: "system", Blocks: results, Status: "complete", CreatedAt: time.Now().UTC()}
		persist, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err = store.Transact(persist, r.deps.DB, func(tx *sql.Tx) error { return service.AppendMessage(persist, tx, &resultMessage) })
		cancel()
		if err != nil {
			return errors.Join(err, toolErr)
		}
		if toolErr != nil {
			return toolErr
		}
		r.deps.Events.Notify()
	}
	return domain.NewError(400, "RUN_MAX_STEPS", "The run reached its step limit.")
}

type streamResult struct {
	usage provider.Usage
	err   error
}

// stream 的唯一消费者维护文本块；计时器即使在上游暂时无数据时也会刷出增量。
func (r *Runtime) stream(parent context.Context, j *job, req provider.Request) (domain.Message, provider.Usage, error) {
	ctx, cancel := context.WithTimeout(parent, r.limits.StepTime)
	defer cancel()
	m := domain.Message{ID: ids.New(), ConversationID: j.run.ConversationID, TurnID: &j.run.TurnID, RunID: &j.run.ID, SenderKind: "bot", SenderBotID: &j.run.BotID, Blocks: []domain.MessageBlock{}, Status: "streaming", CreatedAt: time.Now().UTC()}
	if err := store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error { return service.AppendMessage(ctx, tx, &m) }); err != nil {
		return m, provider.Usage{}, err
	}
	r.deps.Events.Notify()
	events := make(chan provider.Event, 32)
	completed := make(chan streamResult, 1)
	go func() {
		var usage provider.Usage
		call := func(credential []byte) error {
			var err error
			usage, err = r.deps.Factory(j.spec, credential).Stream(ctx, req, func(event provider.Event) error {
				select {
				case events <- event:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			return err
		}
		var err error
		if j.spec.SecretID != nil {
			err = r.deps.Secrets.Use(ctx, *j.spec.SecretID, "bot:"+j.bot.ID, j.run.ID, call)
		} else {
			err = call(nil)
		}
		close(events)
		completed <- streamResult{usage, err}
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deltas := map[int]string{}
	tools := map[string]int{}
	bytesPending, totalBytes := 0, 0
	deliveredFirst := map[string]bool{}
	flush := func(flushCtx context.Context, final bool, usage provider.Usage) error {
		if bytesPending == 0 && !final {
			return nil
		}
		encoded, err := json.Marshal(m.Blocks)
		if err != nil {
			return err
		}
		if final {
			m.Status = "complete"
			m.TokenIn = usage.InputTokens + usage.CacheReadTokens
			m.TokenOut = usage.OutputTokens
		}
		err = store.Transact(flushCtx, r.deps.DB, func(tx *sql.Tx) error {
			if final && j.group != nil && j.mode == "member" {
				// 群成员按完成顺序成为最终发言；流式占位在结束时移动，客户端按同一消息 ID 更新。
				if err := tx.QueryRowContext(flushCtx, "SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?", m.ConversationID).Scan(&m.Seq); err != nil {
					return err
				}
				if _, err := tx.ExecContext(flushCtx, "UPDATE messages SET seq=? WHERE id=?", m.Seq, m.ID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(flushCtx, "UPDATE messages SET blocks_json=?,status=?,token_in=?,token_out=? WHERE id=?", string(encoded), m.Status, m.TokenIn, m.TokenOut, m.ID); err != nil {
				return err
			}
			for index, text := range deltas {
				kind := m.Blocks[index].Kind + ".delta"
				if _, err := bus.Append(flushCtx, tx, kind, m.ConversationID, map[string]any{"run_id": j.run.ID, "message_id": m.ID, "index": index, "text": text}); err != nil {
					return err
				}
			}
			if final {
				if _, err := bus.Append(flushCtx, tx, "message.completed", m.ConversationID, map[string]any{"run_id": j.run.ID, "message_id": m.ID, "usage": usage, "message": m}); err != nil {
					return err
				}
			}
			return appendConversation(flushCtx, tx, m.ConversationID)
		})
		if err == nil {
			clear(deltas)
			bytesPending = 0
			r.deps.Events.Notify()
		}
		return err
	}
	var receiveErr error
	for events != nil && receiveErr == nil {
		select {
		case <-ctx.Done():
			receiveErr = ctx.Err()
		case <-ticker.C:
			receiveErr = flush(ctx, false, provider.Usage{})
		case event, ok := <-events:
			if !ok {
				events = nil
				break
			}
			totalBytes += len(event.Text) + len(event.InputDelta)
			if totalBytes > 4<<20 {
				receiveErr = domain.NewError(502, "PROVIDER_OUTPUT_LIMIT", "The model response exceeds the output limit.")
				break
			}
			firstContent := false
			switch event.Kind {
			case "text_delta", "thinking_delta":
				// 首段正文和思考分别立即提交；之后继续合并落库，使断线快照和实时游标始终一致。
				if event.Text != "" && !deliveredFirst[event.Kind] {
					deliveredFirst[event.Kind] = true
					firstContent = true
				}

				kind := strings.TrimSuffix(event.Kind, "_delta")
				index := len(m.Blocks) - 1
				if index < 0 || m.Blocks[index].Kind != kind {
					m.Blocks = append(m.Blocks, domain.MessageBlock{Kind: kind})
					index++
				}
				m.Blocks[index].Text += event.Text
				deltas[index] += event.Text
				bytesPending += len(event.Text)
			case "tool_use_start":
				if _, duplicate := tools[event.ToolCallID]; duplicate || event.ToolCallID == "" || len(tools) >= 64 {
					receiveErr = domain.NewError(502, "PROVIDER_INVALID_STREAM", "Tool identifiers must be unique and bounded.")
					break
				}
				tools[event.ToolCallID] = len(m.Blocks)
				bytesPending++
				m.Blocks = append(m.Blocks, domain.MessageBlock{Kind: "tool_use", ToolUse: &domain.ToolUseBlock{ID: ids.New(), Name: event.ToolName, Input: map[string]any{}}})
			case "tool_use_end":
				index, ok := tools[event.ToolCallID]
				if !ok {
					receiveErr = domain.NewError(502, "PROVIDER_INVALID_STREAM", "A tool result has no preceding start.")
					break
				}
				var input map[string]any
				if err := json.Unmarshal([]byte(event.InputDelta), &input); err != nil || input == nil {
					receiveErr = domain.NewError(502, "PROVIDER_INVALID_TOOL_INPUT", "Tool arguments must be an object.")
					break
				}
				m.Blocks[index].ToolUse.Input = input
				bytesPending += len(event.InputDelta)
			}
			if receiveErr == nil && (firstContent || bytesPending >= 4096) {
				receiveErr = flush(ctx, false, provider.Usage{})
			}
		}
	}
	if receiveErr != nil {
		cancel()
	}
	result := <-completed
	finalErr := errors.Join(receiveErr, result.err)
	// 取消或网络中断仍保存最后一批文本，但保留 streaming 状态交由收尾标记中断。
	persist, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer persistCancel()
	if err := flush(persist, finalErr == nil, result.usage); err != nil {
		finalErr = errors.Join(finalErr, err)
	}
	return m, result.usage, finalErr
}
