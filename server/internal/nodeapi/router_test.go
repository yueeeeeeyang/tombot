package nodeapi

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
)

type endpointDirectory map[string]Endpoint

func (directory endpointDirectory) Resolve(_ context.Context, id string) (Endpoint, error) {
	endpoint, ok := directory[id]
	if !ok {
		return Endpoint{}, errors.New("节点未登记")
	}
	return endpoint, nil
}

func TestRouterSeparatesNodesAndConnections(t *testing.T) {
	certificate := testClientTLS(t).Certificates[0]
	pin, err := FingerprintOf(certificate)
	if err != nil {
		t.Fatal(err)
	}
	fingerprints, err := ParseFingerprints([]string{pin})
	if err != nil {
		t.Fatal(err)
	}
	directory := endpointDirectory{}
	nodes := map[string]*testNode{}
	for range 2 {
		id := ids.New()
		node := &testNode{}
		nodes[id] = node
		pair := testClientTLS(t).Certificates[0]
		serverTLS, err := ServerTLS(pair, fingerprints)
		if err != nil {
			t.Fatal(err)
		}
		listener, err := Listen("127.0.0.1:0", serverTLS, false)
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(testOperations(node), testOptions(id), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- server.Serve(ctx, listener) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("节点未停止")
			}
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		})
		serverPin, err := FingerprintOf(pair)
		if err != nil {
			t.Fatal(err)
		}
		directory[id] = Endpoint{NodeID: id, URL: "https://" + listener.Addr().String(), Fingerprint: serverPin, ImageID: ids.New()}
	}
	router, err := NewRouter(directory, certificate, time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := router.CloseIdle(); err != nil {
			t.Error(err)
		}
	})
	var calls sync.WaitGroup
	for id := range nodes {
		for range 8 {
			calls.Add(1)
			go func() {
				defer calls.Done()
				heartbeat, err := router.Heartbeat(t.Context(), computer.NodeQuery{NodeID: id})
				if err != nil || heartbeat.NodeID != id {
					t.Error("请求串到其他节点", err)
				}
			}()
		}
	}
	calls.Wait()
	for _, node := range nodes {
		if node.calls.Load() != 8 {
			t.Fatal("节点调用计数错误", node.calls.Load())
		}
	}
	if len(router.clients) != 2 {
		t.Fatal("未复用有界连接池")
	}
	if _, err := router.Heartbeat(t.Context(), computer.NodeQuery{NodeID: ids.New()}); err == nil {
		t.Fatal("未登记节点可调用")
	}
	for id := range nodes {
		endpoint := directory[id]
		endpoint.NodeID = ids.New()
		directory[id] = endpoint
		fresh, err := NewRouter(directory, certificate, time.Second, 2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fresh.Heartbeat(t.Context(), computer.NodeQuery{NodeID: id}); err == nil {
			t.Fatal("连接归属错误仍调用")
		}
		break
	}
}

func TestEndpointURLValidation(t *testing.T) {
	for _, value := range []string{"https://node.example.test", "https://10.0.0.5:9443", "https://[::1]:9443"} {
		if ValidateURL(value) != nil {
			t.Fatal("合法来源被拒绝", value)
		}
	}
	for _, value := range []string{"http://node.example.test", "https://node.example.test/", "https://node.example.test?", "https://name@node.example.test", "https://node.example.test:0", "https://node.example.test:65536", "https://node.example.test:", "https://node.example.test/#part"} {
		if ValidateURL(value) == nil {
			t.Fatal("非法来源被接受", value)
		}
	}
}
