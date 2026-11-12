//go:build linux

package computeragent

import (
	"context"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

func TestKernelProbeProtocol(t *testing.T) {
	fixture := newFixture(t)
	probes, store, factory := newProbeFixture(t, fixture)
	server, err := NewServer(fixture.controller, probes, newTestBrowserController(t, fixture), sameTestProcess, 3*time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 4)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		// 两端具有独立生命周期；客户端收到回包后取消自身调用，不应直接取消服务端的发送收尾。
		go func() { results <- server.serveConnection(t.Context(), right) }()
		return left, nil
	}, sameTestProcess, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	grant := probeGrant(fixture)
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
	proof, err := client.Probe(t.Context(), grant, files)
	if err != nil || proof.Validate(grant.Request.Group.ID) != nil {
		t.Fatal("真实探针描述符通道失败", err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadProbe(t.Context())
	if err != nil || state.Phase != "closed" || !factory.scopes[0].drained.Load() {
		t.Fatal("协议返回前未完成回收", err)
	}
	if _, err := client.Probe(t.Context(), grant, files); err == nil {
		t.Fatal("协议重放探针再次执行")
	}
	if err := <-results; err == nil {
		t.Fatal("重复授权未拒绝")
	}
	// 客户端原描述符仍归节点持有；服务端只接管实际传递的副本。
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatal("客户端原描述符被关闭", err)
		}
	}
}

func TestKernelProbeRejectsForgedResponse(t *testing.T) {
	fixture := newFixture(t)
	grant := probeGrant(fixture)
	for _, stage := range []string{"未回收", "其他版本", "无证据", "失败混合", "其他实例"} {
		t.Run(stage, func(t *testing.T) {
			finished := make(chan error, 1)
			client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
				left, right := protocolPair(t)
				go func() {
					finished <- func() (failure error) {
						channel, _, err := agentchannel.New(right, time.Second)
						if err != nil {
							return err
						}
						defer func() {
							if err := channel.Close(); err != nil {
								failure = err
							}
						}()
						request, files, err := channel.Receive(t.Context())
						if err != nil {
							return err
						}
						if err := agentchannel.CloseFiles(files); err != nil {
							return err
						}
						proof := probeEvidence(grant.Request.Group.ID)
						result := probeResult{Grant: grant, Evidence: &proof, Closed: true}
						switch stage {
						case "未回收":
							result.Closed = false
						case "其他版本":
							result.Grant.Revision++
						case "无证据":
							result.Evidence = nil
						case "失败混合":
							result.Error = "probe_rejected"
						case "其他实例":
							result.Evidence.ServiceID = ""
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
			files := probeFiles(t)
			if _, err := client.Probe(t.Context(), grant, files); err == nil {
				t.Fatal("接受未闭合的探针响应")
			}
			if err := agentchannel.CloseFiles(files); err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}
