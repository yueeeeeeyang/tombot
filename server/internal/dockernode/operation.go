package dockernode

import (
	"context"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
)

// withOperation 沿用活动桌面的锁和租约，所有电脑操作统一受期限约束，调用不会延长执行权。
func (controller *SessionController) withOperation(ctx context.Context, session computer.ExecutionSession, action func(context.Context, *sessionRecord) error) error {
	return controller.withActiveSession(ctx, session, func(ctx context.Context, state *sessionRecord, lease sessionLease) error {
		ctx, cancel := context.WithDeadline(ctx, lease.deadline)
		defer cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		err := action(ctx, state)
		if ctx.Err() != nil || !time.Now().Before(lease.deadline) {
			return errors.Join(err, failure("execution_confirmation_expired"))
		}
		return err
	})
}
