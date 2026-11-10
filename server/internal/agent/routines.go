package agent

import (
	"context"
	"database/sql"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/store"
)

// SubmitRoutine 复用聊天与群路由队列；只有触发关联与回合提交成功后才启动模型，重试不会重放已入队任务。
func (r *Runtime) SubmitRoutine(ctx context.Context, id string) error {
	var trigger domain.RoutineRun
	if err := store.Transact(ctx, r.deps.DB, func(tx *sql.Tx) error { var err error; trigger, err = service.ReadRoutineRun(ctx, tx, id); return err }); err != nil {
		return err
	}
	if trigger.Status != "pending" {
		return nil
	}
	_, err := r.submit(ctx, trigger.ConversationID, SendInput{Content: trigger.Definition.Prompt}, "routine:"+trigger.RoutineID, id)
	return err
}
