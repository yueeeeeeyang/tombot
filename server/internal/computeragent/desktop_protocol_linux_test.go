//go:build linux

package computeragent

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestKernelDesktopProtocol(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingDesktopScope{fakeScope: fixture.factory.scopes[0]}
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
	check := func(wantError bool) {
		t.Helper()
		select {
		case err := <-results:
			if (err != nil) != wantError {
				t.Fatal("图形协议服务端结果不符", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("图形协议请求未结束")
		}
	}
	status, err := client.InspectDesktop(ctx, fixture.command.Session)
	if err != nil || status.State != "absent" {
		t.Fatal(status, err)
	}
	check(false)
	status, err = client.StartDesktop(ctx, fixture.command.Session)
	if err != nil || status.State != "starting" {
		t.Fatal(status, err)
	}
	check(false)
	scope.status.State = "ready"
	scope.status.Width = 1280
	scope.status.Height = 800
	status, err = client.InspectDesktop(ctx, fixture.command.Session)
	if err != nil || status.State != "ready" {
		t.Fatal(status, err)
	}
	check(false)
	scope.status.Width = 1920
	if _, err = client.InspectDesktop(ctx, fixture.command.Session); err == nil {
		t.Fatal("客户端接受了错误桌面分辨率")
	}
	check(false)
	wrong := fixture.command.Session
	wrong.FencingToken++
	if _, err = client.StartDesktop(ctx, wrong); err == nil {
		t.Fatal("另一控制代次启动了桌面")
	}
	check(true)
}
