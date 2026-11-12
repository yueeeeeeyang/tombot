package dockernode

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/processgroup"
)

type fakeAgentAvailability struct{ failure error }

func (probe *fakeAgentAvailability) wait(context.Context, Resource, container) error {
	return probe.failure
}

func testAgentStartupOptions() AgentStartupOptions {
	return AgentStartupOptions{Timeout: time.Second, CallTimeout: time.Second, RetryDelay: 10 * time.Millisecond, MaximumAttempts: 4}
}

func newAgentStartupFixture(t *testing.T) (fixture, *agentStartup, Resource, container) {
	t.Helper()
	fixture, probes, _, resource, current := newProbeFixture(t)
	startup := &agentStartup{node: fixture.node, options: testAgentStartupOptions(), locate: probes.locate}
	startup.check = func(ctx context.Context, _ Resource, _ processgroup.ContainerReference) error { return ctx.Err() }
	return fixture, startup, resource, current
}

func TestAgentStartupWaitsWithBoundedBackoff(t *testing.T) {
	fixture, startup, resource, current := newAgentStartupFixture(t)
	attempts := 0
	var started []time.Time
	startup.check = func(ctx context.Context, _ Resource, _ processgroup.ContainerReference) error {
		attempts++
		started = append(started, time.Now())
		if attempts < 3 {
			return errAgentNotListening
		}
		return ctx.Err()
	}
	if err := startup.wait(t.Context(), resource, current); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || started[1].Sub(started[0]) < startup.options.RetryDelay || started[2].Sub(started[1]) < 2*startup.options.RetryDelay {
		t.Fatal("检查没有按有限退避等待", started)
	}
	if state, err := fixture.node.journal.readBrowser(resource.Specification.UserID); err != nil || state != nil {
		t.Fatal("控制检查创建了浏览器授权", state, err)
	}
}

func TestAgentStartupRejectsNonTransientFailures(t *testing.T) {
	for _, fault := range []error{errors.New("对端身份不符"), errors.New("控制文件权限漂移"), errors.New("响应无法确认"), errors.Join(errAgentNotListening, errors.New("关闭描述符失败"))} {
		_, startup, resource, current := newAgentStartupFixture(t)
		attempts := 0
		startup.check = func(context.Context, Resource, processgroup.ContainerReference) error { attempts++; return fault }
		if err := startup.wait(t.Context(), resource, current); !errors.Is(err, fault) || attempts != 1 {
			t.Fatal("非暂态错误被重试或忽略", attempts, err)
		}
	}
}

func TestAgentStartupDeadlineCancellationAndAttemptLimit(t *testing.T) {
	for _, scenario := range []string{"attempts", "deadline", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			_, startup, resource, current := newAgentStartupFixture(t)
			attempts := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 35*time.Millisecond)
				defer stop()
			}
			if scenario == "cancelled" {
				cancel()
			}
			startup.check = func(ctx context.Context, _ Resource, _ processgroup.ContainerReference) error {
				attempts++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("检查没有时限")
				}
				return errAgentNotListening
			}
			err := startup.wait(ctx, resource, current)
			if err == nil {
				t.Fatal("未监听仍报告成功")
			}
			switch scenario {
			case "attempts":
				if attempts != startup.options.MaximumAttempts {
					t.Fatal("检查没有次数上限", attempts)
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) || attempts >= startup.options.MaximumAttempts {
					t.Fatal("检查延长了调用期限", attempts, err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) || attempts != 0 {
					t.Fatal("取消后仍检查", attempts, err)
				}
			}
		})
	}
}

func TestAgentStartupPinsParentAcrossWaitAndResponse(t *testing.T) {
	for _, duringWait := range []bool{false, true} {
		_, startup, resource, current := newAgentStartupFixture(t)
		attempts := 0
		startup.check = func(context.Context, Resource, processgroup.ContainerReference) error {
			attempts++
			locate := startup.locate
			startup.locate = func(ctx context.Context, id string, pid int) (processgroup.ContainerReference, error) {
				ref, err := locate(ctx, id, pid)
				ref.Inode++
				return ref, err
			}
			if duringWait {
				return errAgentNotListening
			}
			return nil
		}
		if err := startup.wait(t.Context(), resource, current); err == nil || attempts != 1 {
			t.Fatal("父组漂移后继续确认或重试", attempts, err)
		}
	}
}

type gatedBrowsers struct {
	fakeComputerBrowsers
	calls atomic.Int32
}

func (browsers *gatedBrowsers) ensure(ctx context.Context, resource Resource, current container) error {
	browsers.calls.Add(1)
	return browsers.fakeComputerBrowsers.ensure(ctx, resource, current)
}

func TestAgentStartupPrecedesBrowserAndNeverResendsStart(t *testing.T) {
	fixture, startup, _, _ := newAgentStartupFixture(t)
	fixture.node.agent = startup
	browsers := &gatedBrowsers{}
	fixture.node.browsers = browsers
	startup.check = func(context.Context, Resource, processgroup.ContainerReference) error { return errAgentNotListening }
	if _, err := fixture.node.Inspect(t.Context(), fixture.specification); err == nil || browsers.calls.Load() != 0 {
		t.Fatal("监听未确认就启动浏览器", err)
	}
	checks := 0
	startup.check = func(ctx context.Context, _ Resource, _ processgroup.ContainerReference) error {
		checks++
		if browsers.calls.Load() != 0 {
			t.Fatal("重发浏览器启动后又等待监听")
		}
		if checks < 3 {
			return errAgentNotListening
		}
		return ctx.Err()
	}
	injected := errors.New("浏览器启动回包未知")
	browsers.ensureError = injected
	if _, err := fixture.node.Inspect(t.Context(), fixture.specification); !errors.Is(err, injected) || checks != 3 || browsers.calls.Load() != 1 {
		t.Fatal("控制检查与浏览器重试边界错误", checks, browsers.calls.Load(), err)
	}
}

func TestAgentStartupOptionsRejectInvalidBudgets(t *testing.T) {
	for _, change := range []func(*AgentStartupOptions){func(o *AgentStartupOptions) { o.Timeout = 0 }, func(o *AgentStartupOptions) { o.CallTimeout = 2 * time.Second }, func(o *AgentStartupOptions) { o.RetryDelay = time.Millisecond }, func(o *AgentStartupOptions) { o.RetryDelay = 2 * time.Second }, func(o *AgentStartupOptions) { o.MaximumAttempts = 0 }, func(o *AgentStartupOptions) { o.MaximumAttempts = 17 }} {
		options := testAgentStartupOptions()
		change(&options)
		if options.Validate() == nil {
			t.Fatal("接受无效启动预算")
		}
	}
}
