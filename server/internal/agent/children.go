package agent

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

func completedJob(j *job) bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// releaseSlotLocked 只让出模型并发槽，保留 Bot 锁直到原执行完全收尾。
func (r *Runtime) releaseSlotLocked(j *job) {
	if j.holdsSlot {
		j.holdsSlot = false
		r.active--
	}
}

// resumeParent 将等待结束的调用方重新入队，恢复前仍遵守全局并发上限。
func (r *Runtime) resumeParent(ctx context.Context, j *job) error {
	r.mu.Lock()
	if ctx.Err() != nil || r.closed {
		r.mu.Unlock()
		return context.Canceled
	}
	j.resuming = true
	j.resume = make(chan struct{})
	ready := j.resume
	r.queue = append(r.queue, j)
	r.scheduleLocked()
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := r.deps.Events.Publish(ctx, "run.activity", j.run.ConversationID, map[string]any{"run_id": j.run.ID, "turn_id": j.run.TurnID, "bot_id": j.run.BotID, "is_working": true})
		return err
	}
}

// wouldWaitCycleLocked 检查未完成子任务的所有依赖，包含尚未开始等待的异步投递。
func (r *Runtime) wouldWaitCycleLocked(callerID, targetID string) bool {
	queue := []string{targetID}
	seen := map[string]bool{}
	for len(queue) > 0 {
		botID := queue[0]
		queue = queue[1:]
		if botID == callerID {
			return true
		}
		if seen[botID] {
			continue
		}
		seen[botID] = true
		for _, owner := range r.jobs {
			if owner.run.BotID != botID || !owner.started || completedJob(owner) {
				continue
			}
			for _, child := range owner.children {
				if !completedJob(child) {
					queue = append(queue, child.run.BotID)
				}
			}
		}
	}
	return false
}

// drainChildren 在父执行完成前收齐已投递任务，确保费用和取消状态不会在完成事件之后迟到。
func (r *Runtime) drainChildren(ctx context.Context, parent *job) error {
	r.mu.Lock()
	children := append([]*job{}, parent.children...)
	if len(children) > 0 {
		parent.waitingChildren = true
		r.syncHumanClockLocked(parent)
		defer r.finishChildWait(parent)
		r.releaseSlotLocked(parent)
		r.scheduleLocked()
	}
	r.mu.Unlock()
	var result error
	if len(children) > 0 {
		// 收尾只等待已投递任务，不会继续调用模型，不显示成发送者仍在执行。
		_, result = r.deps.Events.Publish(ctx, "run.activity", parent.run.ConversationID, map[string]any{"run_id": parent.run.ID, "turn_id": parent.run.TurnID, "bot_id": parent.run.BotID, "is_working": false})
	}
	for _, child := range children {
		<-child.done
		if child.outcome.persistErr != nil {
			result = errors.Join(result, child.outcome.persistErr)
			continue
		}
		if child.replyRequired || child.outcome.status == "succeeded" || ctx.Err() != nil {
			continue
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := store.Transact(cleanup, r.deps.DB, func(tx *sql.Tx) error {
			message := domain.Message{ID: ids.New(), ConversationID: parent.run.ConversationID, TurnID: &parent.run.TurnID, RunID: &parent.run.ID, SenderKind: "system", Blocks: []domain.MessageBlock{{Kind: "note", Text: "已投递的 Bot 私信未能完成，详情可在对应私信会话查看。"}}, Status: "complete", CreatedAt: time.Now().UTC()}
			return service.AppendMessage(cleanup, tx, &message)
		})
		cancel()
		result = errors.Join(result, err)
		if err == nil {
			r.deps.Events.Notify()
		}
	}
	return result
}
