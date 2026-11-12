//go:build linux

package computeragent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func TestKernelDesktopIOProtocol(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if _, err := fixture.controller.Open(ctx, fixture.command, fixture.reference, fixture.files(t)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	access := newDesktopAccess(fixture.command.Session, func(context.Context) (net.Conn, error) {
		left, right := net.Pipe()
		go func() { done <- serveTestCapture(right, [4]byte{3, 4, 5, 0}) }()
		return left, nil
	})
	defer access.stop()
	fixture.guard.scope = &recordingDesktopIOScope{fakeScope: fixture.factory.scopes[0], access: access}
	server, err := NewServer(fixture.controller, newTestProbeController(t, fixture), newTestBrowserController(t, fixture), sameTestProcess, 2*time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 1)
	client, err := NewClient(func(context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() {
			call, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			results <- server.serveConnection(call, right)
		}()
		return left, nil
	}, sameTestProcess, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantError bool) {
		t.Helper()
		select {
		case err := <-results:
			if (err != nil) != wantError {
				t.Fatal("桌面协议返回错误", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("桌面协议未结束")
		}
	}
	capture := computer.DesktopCapture{Session: fixture.command.Session, Sequence: 1}
	image, err := client.CaptureDesktop(ctx, capture)
	if err != nil || image.Validate(capture) != nil {
		t.Fatal(image, err)
	}
	check(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	read := computer.DesktopImageRead{DesktopCapture: capture, MaximumBytes: computer.MaximumDesktopChunkBytes}
	chunk, err := client.ReadDesktopImage(ctx, read)
	if err != nil || chunk.Validate(read) != nil {
		t.Fatal(err)
	}
	check(false)
	access.dial = func(context.Context) (net.Conn, error) { return nil, errors.New("测试离线") }
	input := computer.DesktopInput{Session: capture.Session, Sequence: 1, Events: []computer.DesktopEvent{{Kind: "pointer", X: 10, Y: 20}}}
	receipt, err := client.InputDesktop(ctx, input)
	if err != nil || receipt.State != "uncertain" {
		t.Fatal(receipt, err)
	}
	check(false)
	input.Session.FencingToken++
	if _, err := client.InputDesktop(ctx, input); err == nil {
		t.Fatal("旧代次输入通过协议")
	}
	check(true)
	read.Session.FencingToken++
	if _, err := client.ReadDesktopImage(ctx, read); err == nil {
		t.Fatal("旧代次读取通过协议")
	}
	check(true)
}
