//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func TestKernelBotAgentProtocol(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingBotScope{fakeScope: fixture.factory.scopes[0]}
	fixture.guard.scope = scope
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 1)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() {
			call, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			results <- server.serveConnection(call, right)
		}()
		return left, nil
	}, sameTestProcess, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantFailure bool) {
		t.Helper()
		select {
		case err := <-results:
			if (err != nil) != wantFailure {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Agent 控制请求未结束")
		}
	}
	if err := client.StartAgent(ctx, fixture.command.Session); err != nil {
		t.Fatal(err)
	}
	check(false)
	payload := bytes.Repeat([]byte{0, 255, 17, 31}, computer.AgentChunkBytes/4)
	offset, err := client.WriteAgent(ctx, computer.AgentWrite{Session: fixture.command.Session, Bytes: payload})
	if err != nil || offset != int64(len(payload)) {
		t.Fatal(offset, err)
	}
	check(false)
	output, err := client.ReadAgent(ctx, computer.AgentRead{Session: fixture.command.Session})
	if err != nil || !bytes.Equal(output.Bytes, payload) {
		t.Fatal("二进制分块损坏", err)
	}
	check(false)
	scope.badOutput = true
	if _, err := client.ReadAgent(ctx, computer.AgentRead{Session: fixture.command.Session}); err == nil {
		t.Fatal("客户端接受了错误偏移")
	}
	check(false)
	wrong := fixture.command.Session
	wrong.FencingToken++
	if err := client.StartAgent(ctx, wrong); err == nil {
		t.Fatal("另一代次启动了 Agent")
	}
	check(true)
}
