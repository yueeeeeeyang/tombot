package orchestrator

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/domain"
)

type Task struct {
	TriggerID   string
	Depth       int
	TurnID      string
	Group       domain.Group
	Content     string
	Mentions    []string
	HistorySeq  int64
	Attachments []domain.ArtifactRef
}
type Delivery struct {
	Task Task
	Step domain.GroupStep
}
type Result struct {
	Followups []Delivery `json:"-"`
	RunID     string     `json:"run_id"`
	BotID     string     `json:"bot_id"`
	Text      string     `json:"text"`
	ToolCalls int        `json:"tool_calls"`
	Status    string     `json:"status"`
	ErrorCode string     `json:"error_code,omitempty"`
}
type Backend interface {
	SavePlan(context.Context, Task, domain.GroupPlan) error
	Run(context.Context, Task, domain.GroupStep) (Result, error)
	CheckBudget(context.Context, Task) error
	Notice(context.Context, Task, string, string) error
}

// WaitingBackend 让人工等待释放编排名额；真实模型并发仍由运行时统一限制。
type WaitingBackend interface {
	WaitingChanges(Task) <-chan struct{}
	WaitingCount(Task) int
}

// Handle 以有界并发调度成员，按完成顺序收集结果；取消时等待已启动执行收尾。
func Handle(ctx context.Context, task Task, backend Backend, concurrency int) error {
	var waitingChanges <-chan struct{}
	waitingCount := func() int { return 0 }
	if waiting, ok := backend.(WaitingBackend); ok {
		waitingChanges = waiting.WaitingChanges(task)
		waitingCount = func() int { return waiting.WaitingCount(task) }
	}
	memberCtx, cancelMembers := context.WithCancel(ctx)
	defer cancelMembers()
	plan := DirectPlan(task.Group, task.Content, task.Mentions)
	var err error
	if err = backend.SavePlan(ctx, task, plan); err != nil {
		return err
	}
	if len(plan.Steps) == 0 {
		return backend.Notice(ctx, task, "GROUP_NO_SPEAKERS", "当前没有可回复的猫咪。")
	}
	type completion struct {
		result Result
		err    error
	}
	completed := make(chan completion, domain.MaxGroupMentionRuns)
	queue := []Delivery{}
	for _, step := range plan.Steps {
		queue = append(queue, Delivery{task, step})
	}
	seen := map[string]bool{}
	for _, delivery := range queue {
		seen[delivery.Task.TriggerID+":"+delivery.Step.BotID] = true
	}
	active, next := 0, 0
	results := []Result{}
	var executionErr error
	for next < len(queue) || active > 0 {
		for active-waitingCount() < max(1, concurrency) && next < len(queue) && executionErr == nil && plan.StopCode == "" && ctx.Err() == nil {
			if err = backend.CheckBudget(ctx, task); err != nil {
				var public *domain.Error
				if errors.As(err, &public) && public.Code == "TURN_BUDGET_EXCEEDED" {
					plan.StopCode = public.Code
				} else {
					executionErr = err
					cancelMembers()
				}
				break
			}
			delivery := queue[next]
			next++
			active++
			go func() {
				result, err := backend.Run(memberCtx, delivery.Task, delivery.Step)
				completed <- completion{result, err}
			}()
		}
		if active == 0 {
			break
		}
		var outcome completion
		select {
		case outcome = <-completed:
		case <-waitingChanges:
			continue
		}
		active--
		if outcome.err != nil {
			executionErr = errors.Join(executionErr, outcome.err)
			cancelMembers()
			continue
		}
		results = append(results, outcome.result)
		for _, delivery := range outcome.result.Followups {
			key := delivery.Task.TriggerID + ":" + delivery.Step.BotID
			if seen[key] {
				continue
			}
			if len(queue) >= domain.MaxGroupMentionRuns || delivery.Task.Depth > domain.MaxGroupMentionDepth {
				plan.StopCode = "GROUP_MENTION_LIMIT"
				continue
			}
			seen[key] = true
			queue = append(queue, delivery)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if executionErr != nil {
		return executionErr
	}
	if plan.StopCode != "" {
		if err = backend.SavePlan(ctx, task, plan); err != nil {
			return err
		}
		text := "本轮成员之间的提及已达到上限，已保留回复。可以发送新消息继续。"
		if plan.StopCode == "TURN_BUDGET_EXCEEDED" {
			text = "已达到群组回合费用上限，停止后续成员执行并保留已完成结果。"
		}
		if err = backend.Notice(ctx, task, plan.StopCode, text); err != nil {
			return err
		}
	}
	for _, result := range results {
		if result.Status != "succeeded" {
			return domain.NewError(502, "GROUP_MEMBER_FAILED", "One or more group members failed. Completed results were preserved.")
		}
	}
	if plan.StopCode == "TURN_BUDGET_EXCEEDED" {
		return domain.NewError(400, "TURN_BUDGET_EXCEEDED", "The group turn stopped at its cost limit.")
	}
	return nil
}
