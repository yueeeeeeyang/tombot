package agent

import (
	"context"
	"errors"

	"io.yueyang/tom/internal/domain"
)

type runUserWaiter struct {
	runtime *Runtime
	job     *job
}

// WaitForUser 保留 Bot 占用但释放模型槽，回答后重新排队；祖先群组的总时限也排除人工等待。
func (w runUserWaiter) WaitForUser(ctx context.Context, wait func(context.Context) (domain.QuestionAnswer, error)) (domain.QuestionAnswer, error) {
	r, j := w.runtime, w.job
	r.mu.Lock()
	if r.closed || r.jobs[j.run.ID] != j || j.clock == nil || ctx.Err() != nil || completedJob(j) {
		r.mu.Unlock()
		return domain.QuestionAnswer{}, domain.NewError(409, "USER_QUESTION_CONTEXT", "A question requires an active run.")
	}
	restore := []func(){}
	ancestors := []*job{}
	j.waitingUser = true
	seen := map[string]bool{}
	for ancestor := j; ancestor != nil; {
		ancestors = append(ancestors, ancestor)
		ancestor.humanWaits++
		r.syncHumanClockLocked(ancestor)
		if group := r.groups[ancestor.run.TurnID]; group != nil && !seen[ancestor.run.TurnID] {
			restore = append(restore, group.clock.Pause())
			seen[ancestor.run.TurnID] = true
		}
		if ancestor.run.ParentRunID == nil {
			break
		}
		ancestor = r.jobs[*ancestor.run.ParentRunID]
	}
	r.releaseSlotLocked(j)
	r.scheduleLocked()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		j.waitingUser = false
		for _, ancestor := range ancestors {
			ancestor.humanWaits--
			r.syncHumanClockLocked(ancestor)
		}
		for i := len(restore) - 1; i >= 0; i-- {
			restore[i]()
		}
	}()
	answer, err := wait(ctx)
	if ctx.Err() != nil {
		return answer, errors.Join(err, ctx.Err())
	}
	return answer, errors.Join(err, r.resumeParent(ctx, j))
}

// syncHumanClockLocked 只有等待该子树回答的祖先暂停，仍在执行模型或工具的父任务继续计时。
func (r *Runtime) syncHumanClockLocked(j *job) {
	if j.clock == nil {
		return
	}
	wasWaiting := j.humanResume != nil
	if j.waitingUser || j.waitingChildren && j.humanWaits > 0 {
		if j.humanResume == nil {
			j.humanResume = j.clock.Pause()
		}
	} else if j.humanResume != nil {
		j.humanResume()
		j.humanResume = nil
	}
	if isWaiting := j.humanResume != nil; isWaiting != wasWaiting && j.group != nil {
		group := r.groups[j.run.TurnID]
		if isWaiting {
			group.waitingRuns++
		} else {
			group.waitingRuns--
		}
		// 通知只要求唤醒，计数在同一把锁下读取；合并事件不会丢失等待状态。
		select {
		case group.waitingChanges <- struct{}{}:
		default:
		}
	}
}

func (r *Runtime) finishChildWait(j *job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j.waitingChildren = false
	r.syncHumanClockLocked(j)
}
