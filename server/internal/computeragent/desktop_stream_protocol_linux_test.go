//go:build linux

package computeragent

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"io.yueyang/tom/internal/computer"
	"net"
	"os"
	"testing"
	"time"
)

type recordingStreamScope struct {
	*fakeScope
	peer net.Conn
}

func (scope *recordingStreamScope) OpenDesktopStreamFile(ctx context.Context, request computer.DesktopStream) (*os.File, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(pair[0]), "viewer")
	peerFile := os.NewFile(uintptr(pair[1]), "desktop")
	scope.peer, err = net.FileConn(peerFile)
	closeError := peerFile.Close()
	if err != nil || closeError != nil {
		return nil, errors.Join(err, closeError, file.Close())
	}
	return file, nil
}
func TestKernelDesktopStreamDescriptorProtocol(t *testing.T) {
	f := newFixture(t)
	if _, err := f.controller.Open(t.Context(), f.command, f.reference, f.files(t)); err != nil {
		t.Fatal(err)
	}
	scope := &recordingStreamScope{fakeScope: f.factory.scopes[0]}
	f.guard.scope = scope
	server, err := NewServer(f.controller, newTestProbeController(t, f), newTestBrowserController(t, f), sameTestProcess, time.Second, 2, f.guard.logger)
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
	request := computer.DesktopStream{Session: f.command.Session, ViewerID: f.command.Session.SessionID}
	connection, err := client.OpenDesktopStream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer scope.peer.Close()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() { _, err := scope.peer.Write([]byte("RFB 003.008\n")); results <- err }()
	banner := make([]byte, 12)
	if _, err := io.ReadFull(connection, banner); err != nil || string(banner) != "RFB 003.008\n" {
		t.Fatal("描述符没有传递原始 RFB 流", err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	request.Session.FencingToken++
	if stream, err := client.OpenDesktopStream(t.Context(), request); err == nil {
		stream.Close()
		t.Fatal("过期代次取得桌面流")
	}
	if err := <-results; err == nil {
		t.Fatal("服务端没有拒绝过期授权")
	}
}
