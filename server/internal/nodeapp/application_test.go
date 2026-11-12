package nodeapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/nodeapi"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestApplicationStopsSupervisionBeforeCleanup(t *testing.T) {
	for _, scenario := range []string{"cancel", "early_exit", "early_error", "ready_failure", "unconfirmed_api"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var workers sync.WaitGroup
			workers.Add(3)
			var order []string
			application := &Application{nodeID: testConfig().NodeID, logger: testLogger(), shutdown: time.Second}
			started := make(chan struct{}, 3)
			for index := range 3 {
				application.services = append(application.services, service{name: "测试监督", run: func(ctx context.Context) error {
					defer workers.Done()
					started <- struct{}{}
					if index == 0 && scenario == "early_exit" {
						return nil
					}
					if index == 0 && scenario == "early_error" {
						return errors.New("测试监督故障")
					}
					<-ctx.Done()
					if index == 0 && scenario == "unconfirmed_api" {
						return nodeapi.ErrUnfinishedOperations
					}
					return ctx.Err()
				}})
			}
			for _, name := range []string{"sessions", "runtime", "storage", "listener"} {
				application.cleanup = append(application.cleanup, cleanupStep{name, func(context.Context) error { workers.Wait(); order = append(order, name); return nil }})
			}
			err := application.Serve(ctx, func(context.Context) error {
				for range 3 {
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatal("监督未启动")
					}
				}
				if scenario == "ready_failure" {
					return errors.New("测试通知故障")
				}
				if scenario == "cancel" || scenario == "unconfirmed_api" {
					cancel()
				}
				return nil
			})
			if (err == nil) != (scenario == "cancel") {
				t.Fatal("退出状态错误", err)
			}
			if scenario == "unconfirmed_api" {
				if !errors.Is(err, ErrProcessExitRequired) || len(order) != 0 {
					t.Fatal("未知在途动作仍释放了依赖", order, err)
				}
			} else if !reflect.DeepEqual(order, []string{"sessions", "runtime", "storage", "listener"}) {
				t.Fatal("释放顺序错误", order)
			}
			if application.Serve(ctx, nil) == nil || application.Close(ctx) == nil {
				t.Fatal("重复运行或释放未拒绝")
			}
		})
	}
}

func TestApplicationPreservesResourcesWhenWorkerStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocked, exited := make(chan struct{}), make(chan struct{})
	defer func() { close(blocked); <-exited }()
	released := false
	application := &Application{logger: testLogger(), shutdown: 20 * time.Millisecond, services: []service{{"未响应监督", func(context.Context) error { defer close(exited); <-blocked; return nil }}}, cleanup: []cleanupStep{{"不可释放", func(context.Context) error { released = true; return nil }}}}
	err := application.Serve(ctx, func(context.Context) error { cancel(); return nil })
	if !errors.Is(err, ErrProcessExitRequired) || !errors.Is(err, context.DeadlineExceeded) || released {
		t.Fatal("监督未停止仍释放依赖", err)
	}
}

func TestCleanupFailureRetainsFollowingResources(t *testing.T) {
	for _, scenario := range []string{"error", "deadline", "late_return"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			released := make(chan struct{}, 1)
			blocked, exited := make(chan struct{}), make(chan struct{})
			step := func(ctx context.Context) error {
				defer close(exited)
				if scenario == "error" {
					return errors.New("测试清理故障")
				}
				if scenario == "deadline" {
					<-ctx.Done()
					return ctx.Err()
				}
				<-blocked
				return nil
			}
			application := &Application{logger: testLogger(), cleanup: []cleanupStep{{"未确认", step}, {"依赖", func(context.Context) error { released <- struct{}{}; return nil }}}}
			err := application.Close(ctx)
			close(blocked)
			<-exited
			if !errors.Is(err, ErrProcessExitRequired) {
				t.Fatal(err)
			}
			select {
			case <-released:
				t.Fatal("释放了未知活动资源的依赖")
			default:
			}
			if application.Close(t.Context()) == nil {
				t.Fatal("重复关闭未拒绝")
			}
		})
	}
}

func TestCloseUnservedApplication(t *testing.T) {
	closed := false
	application := &Application{logger: testLogger(), cleanup: []cleanupStep{{"尚未运行", func(context.Context) error { closed = true; return nil }}}}
	if err := application.Close(t.Context()); err != nil || !closed {
		t.Fatal(err)
	}
}
