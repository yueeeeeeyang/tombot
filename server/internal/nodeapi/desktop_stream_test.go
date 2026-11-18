package nodeapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
)

type streamNode struct {
	*testNode
	expected computer.DesktopStream
}

func (node *streamNode) OpenDesktopStream(ctx context.Context, input computer.DesktopStream) (net.Conn, error) {
	if input != node.expected {
		return nil, errors.New("观看归属不符")
	}
	left, right := net.Pipe()
	go func() {
		defer right.Close()
		buffer := make([]byte, 4096)
		for {
			n, err := right.Read(buffer)
			if err != nil {
				return
			}
			if _, err = right.Write(buffer[:n]); err != nil {
				return
			}
		}
	}()
	return left, nil
}
func TestDesktopStreamPinnedTLSAndShutdown(t *testing.T) {
	session := testSession()
	request := computer.DesktopStream{Session: session, ViewerID: session.SessionID}
	certificate := testClientTLS(t).Certificates[0]
	pin, err := FingerprintOf(certificate)
	if err != nil {
		t.Fatal(err)
	}
	fingerprints, err := ParseFingerprints([]string{pin})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := ServerTLS(certificate, fingerprints)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := Listen("127.0.0.1:0", serverTLS, false)
	if err != nil {
		t.Fatal(err)
	}
	node := &streamNode{testNode: &testNode{}, expected: request}
	operations := testOperations(node.testNode)
	operations.Console = node
	server, err := NewServer(operations, testOptions(session.NodeID), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	clientTLS, err := ClientTLS(certificate, fingerprints)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientOptions{NodeID: session.NodeID, URL: "https://" + listener.Addr().String(), CallTimeout: time.Second, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdle()
	stream, err := client.OpenDesktopStream(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	// 持续连接不受一次动作的 HTTP 调用超时截断。
	time.Sleep(1100 * time.Millisecond)
	expected := bytes.Repeat([]byte("RFB"), 16000)
	sent := make(chan error, 1)
	go func() { _, err := stream.Write(expected); sent <- err }()
	if err := stream.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(stream, actual); err != nil || !bytes.Equal(actual, expected) {
		t.Fatal("跨节点 RFB 字节损坏", err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	foreign := request
	foreign.ViewerID = session.BotID
	if connection, err := client.OpenDesktopStream(ctx, foreign); err == nil {
		connection.Close()
		t.Fatal("错误查看者通过节点入口")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("节点退出没有关闭桌面流")
	}
}
