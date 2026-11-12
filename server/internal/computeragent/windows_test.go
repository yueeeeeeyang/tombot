package computeragent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

func TestWindowSourcePinsActiveInstance(t *testing.T) {
	fixture := newFixture(t)
	controller, _, _ := newBrowserControlFixture(t, fixture)
	session := fixture.command.Session
	if _, err := controller.windowSource(session); err == nil {
		t.Fatal("共享服务未启动就提供窗口入口")
	}
	grant := browserGrant(fixture)
	if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := controller.windowSource(session)
	if err != nil || first.service == nil {
		t.Fatal("活动共享服务不可绑定", err)
	}
	for _, change := range []func(*computer.ExecutionSession){
		func(s *computer.ExecutionSession) { s.UserID = ids.New() },
		func(s *computer.ExecutionSession) { s.ComputerID = ids.New() },
		func(s *computer.ExecutionSession) { s.NodeID = ids.New() },
		func(s *computer.ExecutionSession) { s.Generation++ },
		func(s *computer.ExecutionSession) { s.ContainerID = strings.Repeat("b", 64) },
	} {
		wrong := session
		change(&wrong)
		if _, err := controller.windowSource(wrong); err == nil {
			t.Fatal("其他实例身份获得共享浏览器")
		}
	}
	if _, err := controller.Close(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.windowSource(session); err == nil || !first.stopping.Load() {
		t.Fatal("关闭之后仍发放原窗口入口")
	}
	grant.Revision++
	grant.Request.Group.ID = ids.New()
	if _, err := controller.startAt(t.Context(), grant, probeFiles(t), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := controller.windowSource(session)
	if err != nil || second == first || !first.stopping.Load() {
		t.Fatal("新实例复用了旧窗口绑定", err)
	}
	controller.fail()
	if _, err := controller.windowSource(session); err == nil {
		t.Fatal("共享服务失控后继续发放入口")
	}
}

type shutdownFunc func(context.Context) error

func (close shutdownFunc) Shutdown(ctx context.Context) error { return close(ctx) }

func TestWindowShutdownOrdersSharedServiceLast(t *testing.T) {
	for _, scenario := range []string{"success", "desktop_failure", "probe_failure", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var completed, browserCalls atomic.Int32
			fault := errors.New("所属资源未回收")
			dependency := func(kind string) shutdownFunc {
				return func(ctx context.Context) error {
					if scenario == "cancelled" {
						<-ctx.Done()
						return ctx.Err()
					}
					completed.Add(1)
					if scenario == kind+"_failure" {
						return fault
					}
					return nil
				}
			}
			browser := shutdownFunc(func(context.Context) error {
				browserCalls.Add(1)
				if completed.Load() != 2 {
					t.Error("会话页面尚未回收就结束共享服务")
				}
				return nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			err := shutdownControllers(ctx, dependency("desktop"), dependency("probe"), browser)
			if scenario == "success" {
				if err != nil || browserCalls.Load() != 1 {
					t.Fatal("正常退出没有按依赖顺序关闭", err)
				}
			} else if err == nil || browserCalls.Load() != 0 {
				t.Fatal("依赖回收失败后仍释放共享服务", err)
			}
		})
	}
}
