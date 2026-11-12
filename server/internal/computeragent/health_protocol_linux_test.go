//go:build linux

package computeragent

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"io.yueyang/tom/internal/agentchannel"
)

func TestKernelControlHealthIsReadOnlyAndStopsWithAgent(t *testing.T) {
	fixture := newFixture(t)
	desktopCount := len(fixture.controller.desktops)
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, time.Second, 4, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 1)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() { results <- server.serveConnection(t.Context(), right) }()
		return left, nil
	}, sameTestProcess, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	identity := fixture.guard.identity
	for range 2 {
		if err := client.CheckControl(t.Context(), identity); err != nil {
			t.Fatal(err)
		}
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(fixture.controller.desktops) != desktopCount || len(fixture.factory.scopes) != 0 {
		t.Fatal("控制检查创建了桌面")
	}
	other := identity
	other.Generation++
	if err := client.CheckControl(t.Context(), other); err == nil {
		t.Fatal("接受其他实例身份")
	}
	if err := <-results; err == nil {
		t.Fatal("服务未拒绝错误身份")
	}
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	var reply healthResult
	if err := client.exchange(t.Context(), "health", identity, []*os.File{file}, &reply); err != nil {
		t.Fatal(err)
	}
	if err := <-results; err == nil || reply.Error == "" || reply.Observation != nil {
		t.Fatal("控制检查接受描述符", reply, err)
	}
	fixture.controller.fail()
	if err := client.CheckControl(t.Context(), identity); err == nil {
		t.Fatal("失控后仍报告可接单")
	}
	if err := <-results; err == nil {
		t.Fatal("失控未拒绝检查")
	}
}

func TestKernelControlHealthRejectsForgedObservation(t *testing.T) {
	fixture := newFixture(t)
	identity := fixture.guard.identity
	for _, scenario := range []string{"scope", "identity", "version", "missing", "mixed"} {
		t.Run(scenario, func(t *testing.T) {
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
						observation := controlHealth{Identity: identity, Scope: "agent_control", Version: ConfigurationVersion}
						result := healthResult{Observation: &observation}
						switch scenario {
						case "scope":
							observation.Scope = "computer"
						case "identity":
							observation.Identity.Generation++
						case "version":
							observation.Version++
						case "missing":
							result.Observation = nil
						case "mixed":
							result.Error = "control_unavailable"
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
			if err := client.CheckControl(t.Context(), identity); err == nil {
				t.Fatal("接受伪造控制响应")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
