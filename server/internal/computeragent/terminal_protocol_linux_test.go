//go:build linux

package computeragent

import (
	"context"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

func TestKernelTerminalProtocol(t *testing.T) {
	fixture, scope, request := terminalFixture(t)
	server, err := NewServer(fixture.controller, newTestProbeController(t, *fixture), newTestBrowserController(t, *fixture), sameTestProcess, time.Second, 2, fixture.guard.logger)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 1)
	client, err := NewClient(func(ctx context.Context) (*net.UnixConn, error) {
		left, right := protocolPair(t)
		go func() {
			call, cancel := context.WithTimeout(context.Background(), time.Second)
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
				t.Fatal("终端协议服务端结果不符", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("终端协议请求没有结束")
		}
	}
	ctx := context.Background()
	output, err := client.StartTerminal(ctx, request)
	if err != nil || output.Text != "中文" {
		t.Fatal("终端启动协议失败", err, output)
	}
	check(false)
	output, err = client.ReadTerminal(ctx, computer.TerminalRead{TerminalAddress: request.TerminalAddress, MaximumBytes: 4096})
	if err != nil || output.Cursor != 6 {
		t.Fatal("终端读取协议失败", err, output)
	}
	check(false)
	receipt, err := client.WriteTerminal(ctx, computer.TerminalWrite{TerminalAddress: request.TerminalAddress, Sequence: 1, Text: "你好\n"})
	if err != nil || receipt.WrittenBytes != 7 {
		t.Fatal("终端输入协议失败", err, receipt)
	}
	check(false)
	if err := client.ResizeTerminal(ctx, computer.TerminalResize{TerminalAddress: request.TerminalAddress, Columns: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	check(false)
	wrong := request.TerminalAddress
	wrong.Session.FencingToken++
	if _, err := client.ReadTerminal(ctx, computer.TerminalRead{TerminalAddress: wrong, MaximumBytes: 4096}); err == nil {
		t.Fatal("另一代会话读取了终端")
	}
	check(true)
	if scope.calls != 4 {
		t.Fatal("拒绝的请求进入了执行器")
	}
}
