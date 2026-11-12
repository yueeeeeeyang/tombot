//go:build linux

package computeragent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

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
