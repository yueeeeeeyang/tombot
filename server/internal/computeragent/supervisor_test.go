package computeragent

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisorIndependentFatalExit(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	var stopped, closed atomic.Bool
	result := make(chan error, 1)
	go func() {
		result <- supervise(context.Background(), time.Second,
			func(context.Context) error { <-blocked; return nil },
			func(context.Context) error { return errors.New("整组终止失败") },
			func(context.Context) error { stopped.Store(true); <-blocked; return nil },
			func() error { closed.Store(true); return nil })
	}()
	select {
	case err := <-result:
		if err == nil || stopped.Load() || closed.Load() {
			t.Fatal("致命故障等待阻塞清理或提前释放持久锁", err)
		}
	case <-time.After(time.Second):
		t.Fatal("看门狗致命故障被控制请求阻塞")
	}
}

func TestSupervisorGracefulAndBoundedShutdown(t *testing.T) {
	t.Run("正常停服先清理再释放", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var served, watched, stopped, closed atomic.Bool
		wait := func(flag *atomic.Bool) func(context.Context) error {
			return func(ctx context.Context) error { <-ctx.Done(); flag.Store(true); return ctx.Err() }
		}
		result := make(chan error, 1)
		go func() {
			result <- supervise(ctx, time.Second, wait(&served), wait(&watched),
				func(context.Context) error { stopped.Store(true); return nil },
				func() error {
					if !served.Load() || !watched.Load() || !stopped.Load() {
						return errors.New("释放顺序错误")
					}
					closed.Store(true)
					return nil
				})
		}()
		cancel()
		select {
		case err := <-result:
			if err != nil || !closed.Load() {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("正常停服未完成")
		}
	})
	for _, phase := range []string{"请求", "清理", "关闭记录"} {
		t.Run(phase+"阻塞时有界退出", func(t *testing.T) {
			blocked := make(chan struct{})
			defer close(blocked)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			wait := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			serve := wait
			shutdown := func(context.Context) error { return nil }
			closeResources := func() error { return nil }
			switch phase {
			case "请求":
				serve = func(context.Context) error { <-blocked; return context.Canceled }
			case "清理":
				shutdown = func(context.Context) error { <-blocked; return nil }
			case "关闭记录":
				closeResources = func() error { <-blocked; return nil }
			}
			result := make(chan error, 1)
			go func() { result <- supervise(ctx, 50*time.Millisecond, serve, wait, shutdown, closeResources) }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("阻塞停服被报告成功")
				}
			case <-time.After(time.Second):
				t.Fatal("停服超过独立期限")
			}
		})
	}
}

func TestAgentRuntimeOptionsBoundaries(t *testing.T) {
	valid := RuntimeOptions{EgressConnectTimeoutMS: 1000, EgressIdleTimeoutMS: 1000, EgressLifetimeMS: 10000, EgressMaximumConnections: 8, EgressMaximumTransferBytes: 1 << 20, MaximumActive: 4, CallTimeoutMS: 1000, CleanupTimeoutMS: 1000, RetryDelayMS: 10, CleanupAttempts: 2, PollIntervalMS: 10, ShutdownTimeoutMS: 3000, MaximumConnections: 8}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*RuntimeOptions){
		func(o *RuntimeOptions) { o.CallTimeoutMS = 0 }, func(o *RuntimeOptions) { o.CleanupTimeoutMS = 60001 },
		func(o *RuntimeOptions) { o.RetryDelayMS = -1 }, func(o *RuntimeOptions) { o.CleanupAttempts = 6 },
		func(o *RuntimeOptions) { o.PollIntervalMS = 0 }, func(o *RuntimeOptions) { o.ShutdownTimeoutMS = 1999 },
		func(o *RuntimeOptions) { o.ShutdownTimeoutMS = 2999 },
		func(o *RuntimeOptions) { o.MaximumConnections = 129 },
		func(o *RuntimeOptions) { o.MaximumActive = 0 }, func(o *RuntimeOptions) { o.MaximumActive = 65 },
		func(o *RuntimeOptions) { o.EgressConnectTimeoutMS = 0 }, func(o *RuntimeOptions) { o.EgressIdleTimeoutMS = 0 }, func(o *RuntimeOptions) { o.EgressLifetimeMS = 999 }, func(o *RuntimeOptions) { o.EgressMaximumConnections = 0 }, func(o *RuntimeOptions) { o.EgressMaximumTransferBytes = 0 },
		func(o *RuntimeOptions) { o.EgressConnectTimeoutMS = math.MaxInt }, func(o *RuntimeOptions) { o.EgressIdleTimeoutMS = math.MaxInt }, func(o *RuntimeOptions) { o.EgressLifetimeMS = math.MaxInt },
	} {
		candidate := valid
		change(&candidate)
		if candidate.Validate() == nil {
			t.Fatal("运行限制未校验", candidate)
		}
	}
}
