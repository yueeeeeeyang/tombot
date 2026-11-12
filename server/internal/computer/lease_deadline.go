package computer

import (
	"context"
	"time"
)

type leaseDeadlineKey struct{}

// WithLeaseDeadline 供受信节点入口传入已核验的跨进程绝对租约；不使用单次 RPC 的超时代替执行租约。
func WithLeaseDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, leaseDeadlineKey{}, deadline)
}

// LeaseDeadline 保留本机直接调用语义，跨进程入口只能缩短期限；队列和传输耗时不重新获得完整租期。
func LeaseDeadline(ctx context.Context, lifetime time.Duration) time.Time {
	deadline := time.Now().Add(lifetime)
	if received, ok := ctx.Value(leaseDeadlineKey{}).(time.Time); ok && received.Before(deadline) {
		return received
	}
	return deadline
}
