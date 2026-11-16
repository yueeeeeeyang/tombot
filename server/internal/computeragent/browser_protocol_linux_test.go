//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/browserprobe"
	"io.yueyang/tom/internal/sharedbrowser"
	"io.yueyang/tom/internal/taskprocess"
)

func TestKernelBrowserMalformedStartupReturnsFixedClassification(t *testing.T) {
	fixture := newFixture(t)
	browsers, store, factory := newBrowserControlFixture(t, fixture)
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), browsers, sameTestProcess, 3*time.Second, 4, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() { done <- server.serveConnection(t.Context(), right) }()
		return left, nil
	}, sameTestProcess, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := agentchannel.Encode(browserGrant(fixture))
	if err != nil {
		t.Fatal(err)
	}
	// 重复字段能解码为合法结构，但必须在接管任何入口前拒绝。
	encoded = bytes.Replace(encoded, []byte(`"revision":1`), []byte(`"revision":1,"revision":1`), 1)
	var result browserResult
	if err := client.exchange(t.Context(), "browser_start", json.RawMessage(encoded), nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Observation != nil || result.Error != "browser_start_request_failed" {
		t.Fatal("格式拒绝未返回固定分类", result)
	}
	if err := <-done; err == nil || len(factory.scopes) != 0 || store.state != nil {
		t.Fatal("非法格式请求被接管或消费", err)
	}
}

func TestKernelBrowserPreflightRejectionsCrossProtocol(t *testing.T) {
	for _, test := range []struct {
		name, code string
		change     func(*BrowserGrant, *BrowserController)
	}{
		{"过期时钟", "browser_start_deadline_failed", func(grant *BrowserGrant, _ *BrowserController) { grant.Request.Deadline.ExpiresNS = 1 }},
		{"其他时间命名空间", "browser_start_deadline_failed", func(grant *BrowserGrant, _ *BrowserController) { grant.Request.Deadline.TimeNamespace++ }},
		{"其他容器代次", "browser_start_identity_failed", func(grant *BrowserGrant, _ *BrowserController) { grant.Identity.Generation++ }},
		{"调用预算超限", "browser_start_budget_failed", func(grant *BrowserGrant, _ *BrowserController) { grant.Limits.CallTimeoutMS = 2000 }},
		{"回收预算超限", "browser_start_budget_failed", func(grant *BrowserGrant, _ *BrowserController) { grant.Limits.ShutdownTimeoutMS = 2000 }},
		{"启动授权已消费", "browser_start_state_failed", func(grant *BrowserGrant, controller *BrowserController) {
			controller.state = &BrowserSnapshot{Version: 1, Grant: *grant, Phase: "closed"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			browsers, store, factory := newBrowserControlFixture(t, fixture)
			server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), browsers, sameTestProcess, 3*time.Second, 4, fixture.guard.logger)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
				left, right := protocolPair(t)
				go func() { done <- server.serveConnection(t.Context(), right) }()
				return left, nil
			}, sameTestProcess, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			grant := browserGrant(fixture)
			grant.Request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(3 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			test.change(&grant, browsers)
			files := probeFiles(t)
			defer func() {
				if err := agentchannel.CloseFiles(files); err != nil {
					t.Error(err)
				}
			}()
			observed, err := client.StartBrowser(t.Context(), grant, files)
			var failure *browserprobe.Failure
			if !errors.As(err, &failure) || failure.Code != test.code || err.Error() != failure.Error() || observed != (BrowserObservation{}) {
				t.Fatal("启动前拒绝丢失分类、泄漏原文或返回成功", observed, err)
			}
			if err := <-done; err == nil {
				t.Fatal("服务端未保留拒绝结果")
			}
			if len(factory.scopes) != 0 || store.state != nil {
				t.Fatal("拒绝请求仍接管资源或消费授权")
			}
			for _, file := range files {
				if _, err := file.Stat(); err != nil {
					t.Fatal("拒绝请求关闭了宿主保留的入口", err)
				}
			}
		})
	}
}

func TestKernelBrowserStartupFailureCrossesProtocolWithoutRawOutput(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(map[bool]string{false: "正常失败回执", true: "致命退出丢失回执"}[fatal], func(t *testing.T) {
			fixture := newFixture(t)
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := openFileStore(directory, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			browsers, _, factory := newBrowserControlFixture(t, fixture)
			browsers.store = store
			startup := browserprobe.StartFailure("browser_start_membership_failed", errors.New("不应跨进程传递的底层诊断"))
			factory.prepare = func(scope *fakeBrowserScope) {
				scope.start = func(context.Context, taskprocess.BrowserRequest, sharedbrowser.Options) (BrowserService, error) {
					return nil, startup
				}
				if fatal {
					scope.drain = func(context.Context) error { return startup }
				}
			}
			server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), browsers, sameTestProcess, 3*time.Second, 4, fixture.guard.logger)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
				left, right := protocolPair(t)
				if fatal {
					// 模拟生产看门狗立即退出：不等待错误回执，也不放松致命退出条件。
					browsers.onFatal = func() {
						if err := right.Close(); err != nil {
							t.Error(err)
						}
						fixture.controller.fail()
					}
				}
				go func() { done <- server.serveConnection(t.Context(), right) }()
				return left, nil
			}, sameTestProcess, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			grant := browserGrant(fixture)
			grant.Request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(3 * time.Second))
			if err != nil {
				t.Fatal(err)
			}
			files := probeFiles(t)
			defer func() {
				if err := agentchannel.CloseFiles(files); err != nil {
					t.Error(err)
				}
			}()
			observed, err := client.StartBrowser(t.Context(), grant, files)
			var failed *browserprobe.Failure
			if err == nil || observed.Phase != "" {
				t.Fatal("失败启动被报告成功", err)
			}
			if fatal {
				if errors.As(err, &failed) {
					t.Fatal("测试没有复现代理退出导致的回执丢失")
				}
			} else if !errors.As(err, &failed) || failed.Code != "browser_start_membership_failed" || err.Error() != failed.Error() {
				t.Fatal("协议丢失分类或泄漏原始错误", err)
			}
			if err := <-done; err == nil {
				t.Fatal("服务端把失败启动当作成功")
			}
			snapshot, err := store.LoadBrowser(t.Context())
			if err != nil || snapshot == nil || snapshot.Grant != grant || snapshot.StartupError != "browser_start_membership_failed" {
				t.Fatal("回执丢失后未保留本次授权的失败原因", snapshot, err)
			}
			if factory.scopes[0].drained.Load() == fatal || fatal && snapshot.Phase != "consumed" || !fatal && snapshot.Phase != "closed" {
				t.Fatal("诊断改变了原服务组的关闭状态", snapshot)
			}
		})
	}
}

func TestKernelBrowserControlProtocol(t *testing.T) {
	fixture := newFixture(t)
	browsers, store, factory := newBrowserControlFixture(t, fixture)
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), browsers, sameTestProcess, 3*time.Second, 4, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() { results <- server.serveConnection(t.Context(), right) }()
		return left, nil
	}, sameTestProcess, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	grant := browserGrant(fixture)
	grant.Request.Deadline, err = agentchannel.FromDeadline(time.Now().Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	files := probeFiles(t)
	t.Cleanup(func() {
		if err := agentchannel.CloseFiles(files); err != nil {
			t.Error(err)
		}
	})
	observed, err := client.StartBrowser(t.Context(), grant, files)
	if err != nil || observed.Grant != grant || observed.Phase != "active" {
		t.Fatal(observed, err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartBrowser(t.Context(), grant, files); err == nil {
		t.Fatal("协议重放再次启动")
	}
	if err := <-results; err == nil {
		t.Fatal("服务端接受重复启动")
	}
	if observed, err = client.BrowserStatus(t.Context(), grant); err != nil || observed.Phase != "active" {
		t.Fatal(observed, err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if observed, err = client.CloseBrowser(t.Context(), grant); err != nil || observed.Phase != "closed" {
		t.Fatal(observed, err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadBrowser(t.Context())
	if err != nil || state.Phase != "closed" || !factory.scopes[0].drained.Load() {
		t.Fatal(state, err)
	}
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatal("协议关闭了节点原入口", err)
		}
	}
}

func TestKernelBrowserControlRejectsForgedResponse(t *testing.T) {
	fixture := newFixture(t)
	grant := browserGrant(fixture)
	for _, phase := range []string{"无状态", "错误授权", "错误阶段", "失败混合"} {
		t.Run(phase, func(t *testing.T) {
			done := make(chan error, 1)
			client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
				left, right := protocolPair(t)
				go func() {
					done <- func() (failure error) {
						channel, _, err := agentchannel.New(right, time.Second)
						if err != nil {
							return err
						}
						defer func() { failure = errors.Join(failure, channel.Close()) }()
						request, files, err := channel.Receive(t.Context())
						if err != nil {
							return err
						}
						if err := agentchannel.CloseFiles(files); err != nil {
							return err
						}
						observed := BrowserObservation{Grant: grant, Phase: "active"}
						result := browserResult{Observation: &observed}
						switch phase {
						case "无状态":
							result.Observation = nil
						case "错误授权":
							observed.Grant.Revision++
						case "错误阶段":
							observed.Phase = "consumed"
						case "失败混合":
							result.Error = "browser_rejected"
						}
						encoded, err := agentchannel.Encode(result)
						if err != nil {
							return err
						}
						return channel.Send(t.Context(), agentchannel.Message{Version: 1, ID: request.ID, Method: "result", Body: encoded}, nil)
					}()
				}()
				return left, nil
			}, sameTestProcess, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.BrowserStatus(t.Context(), grant); err == nil {
				t.Fatal("接受伪造浏览器回执")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
