package egressproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func bridgeOptions() BridgeOptions {
	return BridgeOptions{ConnectTimeout: time.Second, IdleTimeout: time.Second, Lifetime: 3 * time.Second, ShutdownTimeout: 2 * time.Second, MaximumConnections: 2, MaximumTransferBytes: 1 << 20}
}

func startBridge(t *testing.T, dial func(context.Context) (net.Conn, error)) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	bridge, err := NewBridge(bridgeOptions(), dial, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() { finished <- bridge.Serve(ctx, listener); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("转接服务未结束")
		}
	})
	return listener.Addr().String(), cancel, finished
}

func TestBridgeCarriesRequestsAndStops(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	address, cancel, finished := startBridge(t, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	})
	endpoint, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint)}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get("http://web.example.test/path")
	if err != nil {
		t.Fatal(err)
	}
	readResponse(t, response)
	if response.StatusCode != 204 {
		t.Fatal("转接结果不符")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("转接取消错误", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("转接停服未完成")
	}
}

func TestBridgeUpstreamFailureAndCapacity(t *testing.T) {
	var attempts atomic.Int32
	address, _, _ := startBridge(t, func(context.Context) (net.Conn, error) {
		attempts.Add(1)
		return nil, errors.New("测试节点不可用")
	})
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	readResponse(t, response)
	if response.StatusCode != 502 || attempts.Load() != 1 {
		t.Fatal("节点失败被重连或隐瞒")
	}
	started := make(chan struct{}, 2)
	address, cancel, finished := startBridge(t, func(ctx context.Context) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	connections := []net.Conn{}
	t.Cleanup(func() {
		if err := closeNetworkConnections(connections...); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("转接未开始连接")
		}
	}
	third, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	connections = append(connections, third)
	if err := third.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := third.Read(one[:]); err == nil {
		t.Fatal("超额连接未关闭")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("连接中的转接无法取消")
	}
}

func TestBridgeRequiresLoopbackAndBounds(t *testing.T) {
	for _, change := range []func(*BridgeOptions){func(o *BridgeOptions) { o.ConnectTimeout = 0 }, func(o *BridgeOptions) { o.IdleTimeout = 0 }, func(o *BridgeOptions) { o.Lifetime = time.Nanosecond }, func(o *BridgeOptions) { o.MaximumConnections = 0 }, func(o *BridgeOptions) { o.MaximumTransferBytes = 0 }, func(o *BridgeOptions) { o.ShutdownTimeout = time.Millisecond }} {
		options := bridgeOptions()
		change(&options)
		if options.Validate() == nil {
			t.Fatal("无界转接配置未拒绝")
		}
	}
	bridge, err := NewBridge(bridgeOptions(), func(context.Context) (net.Conn, error) { return nil, ErrUnavailable }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := bridge.Serve(t.Context(), listener); err == nil {
		t.Fatal("转接监听了所有接口")
	}
	if RequireHostConnection(nil) == nil {
		t.Fatal("缺少宿主连接仍验证通过")
	}
}
