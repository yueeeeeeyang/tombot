package egressproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"io.yueyang/tom/internal/infra/ids"
)

type resolveFunc func(context.Context, string, string) ([]netip.Addr, error)

func (call resolveFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return call(ctx, network, host)
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (call dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return call(ctx, network, address)
}

func testOptions() Options {
	return Options{Ports: []uint16{80, 443}, ConnectTimeout: time.Second, HeaderTimeout: time.Second, IdleTimeout: time.Second, RequestTimeout: 3 * time.Second, TunnelTimeout: 3 * time.Second, ShutdownTimeout: 2 * time.Second, MaximumConnections: 8, MaximumRequestBytes: 4096, MaximumResponseBytes: 4096, MaximumTunnelBytes: 8192}
}
func testProxy(t *testing.T) *Server {
	t.Helper()
	proxy, err := New(testOptions(), Owner{UserID: ids.New(), ComputerID: ids.New(), ContainerID: strings.Repeat("a", 64), Generation: 1}, func(context.Context, Peer) error { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy.resolver = resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	proxy.localAddresses = func() ([]netip.Addr, error) { return nil, nil }
	return proxy
}

// startHTTPProxy 仅测试 HTTP 语义；生产端点必须经过 Linux 内核身份检查，另有真实 Unix 进程测试。
func startHTTPProxy(t *testing.T, proxy *Server) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.handle(w, r.WithContext(context.WithValue(r.Context(), authorizedKey{}, true)))
	}))
	t.Cleanup(server.Close)
	return server
}
func routedClient(t *testing.T, server *httptest.Server, configuration *tls.Config) *http.Client {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(endpoint), TLSClientConfig: configuration, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func pointAt(proxy *Server, address string, calls *atomic.Int32) {
	proxy.dialer = dialFunc(func(ctx context.Context, network, destination string) (net.Conn, error) {
		calls.Add(1)
		if network != "tcp" || destination != "8.8.8.8:80" && destination != "8.8.8.8:443" {
			return nil, errors.New("测试发现连接未固定为核验后的数字地址")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
}
func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	content, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatal("测试响应未完成", err, closeErr)
	}
	return string(content)
}

func TestHTTPAndTLSForwarding(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			var received atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				if r.Host != "web.example.test" || r.URL.Path != "/submit" || !secure && (r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Hop") != "") {
					t.Error("转发目标或逐跳头未规范化")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if !secure {
					connection, buffer, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := fmt.Fprintf(buffer, "HTTP/1.1 200 OK\r\nConnection: X-Private, close\r\nX-Private: hidden\r\nX-Result: visible\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
						t.Error(err)
					}
					if err := errors.Join(buffer.Flush(), connection.Close()); err != nil {
						t.Error(err)
					}
					return
				}
				w.Header().Set("Connection", "X-Private")
				w.Header().Set("X-Private", "hidden")
				w.Header().Set("X-Result", "visible")
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			})
			upstream := httptest.NewUnstartedServer(handler)
			var trust *tls.Config
			if secure {
				upstream.StartTLS()
				trust = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				trust.ServerName = "example.com"
			} else {
				upstream.Start()
			}
			t.Cleanup(upstream.Close)
			proxy := testProxy(t)
			var calls atomic.Int32
			pointAt(proxy, upstream.Listener.Addr().String(), &calls)
			server := startHTTPProxy(t, proxy)
			client := routedClient(t, server, trust)
			scheme := "http"
			if secure {
				scheme = "https"
			}
			r, err := http.NewRequest("POST", scheme+"://web.example.test/submit", strings.NewReader("共享电脑传输"))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Connection", "X-Hop")
			r.Header.Set("X-Hop", "remove")
			if !secure {
				r.Header.Set("Proxy-Authorization", "测试代理头")
			}
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || readResponse(t, response) != "共享电脑传输" || response.Header.Get("X-Result") != "visible" {
				t.Fatal("传输响应不符")
			}
			// CONNECT 保留端到端 TLS 内容；逐跳头只由明文 HTTP 路径清理。
			if !secure && response.Header.Get("X-Private") != "" {
				t.Fatal("泄露上游逐跳头")
			}
			if calls.Load() != 1 || received.Load() != 1 {
				t.Fatal("请求重复发送")
			}
		})
	}
}

func TestAddressPolicy(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		addresses []string
		denied    []netip.Prefix
		local     []netip.Addr
	}{
		{"环回", []string{"127.0.0.1"}, nil, nil},
		{"私网", []string{"10.0.0.1"}, nil, nil},
		{"映射私网", []string{"::ffff:127.0.0.1"}, nil, nil},
		{"云元数据", []string{"169.254.169.254"}, nil, nil},
		{"云平台地址", []string{"168.63.129.16"}, nil, nil},
		{"转换地址", []string{"64:ff9b::808:808"}, nil, nil},
		{"混合解析", []string{"8.8.8.8", "192.168.1.2"}, nil, nil},
		{"部署排除", []string{"8.8.8.8"}, []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}, nil},
		{"宿主公网地址", []string{"8.8.8.8"}, nil, []netip.Addr{netip.MustParseAddr("8.8.8.8")}},
		{"无结果", nil, nil, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			proxy := testProxy(t)
			proxy.options.DeniedNetworks = fixture.denied
			proxy.resolver = resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				values := []netip.Addr{}
				for _, address := range fixture.addresses {
					values = append(values, netip.MustParseAddr(address))
				}
				return values, nil
			})
			proxy.localAddresses = func() ([]netip.Addr, error) { return fixture.local, nil }
			proxy.dialer = dialFunc(func(context.Context, string, string) (net.Conn, error) {
				t.Error("禁止目标仍调用连接器")
				return nil, ErrUnavailable
			})
			if _, err := proxy.connect(t.Context(), destination{"web.example.test", "443"}); !errors.Is(err, ErrAddressDenied) {
				t.Fatal("未拒绝目标", err)
			}
		})
	}
	for _, authority := range []string{"", "web.test:0", "web.test:99999", "web.test:", "user@web.test:80", "web.test:080", "web.test/path", "web.test#fragment", "web.test%00:80", "[fe80::1%lo0]:80", "web..test", "web.test.", "-bad.test", "web.test\\x"} {
		if _, err := target(authority, "80"); err == nil {
			t.Fatalf("接受错误目标 %q", authority)
		}
	}
	for _, authority := range []string{"web.example.test", "web.example.test:80", "[2606:4700:4700::1111]:443"} {
		if _, err := target(authority, "80"); err != nil {
			t.Fatal("正常目标未通过", authority, err)
		}
	}
}

func TestDNSRevalidationAndNoReplay(t *testing.T) {
	var delivered atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { delivered.Add(1); w.WriteHeader(204) }))
	defer upstream.Close()
	proxy := testProxy(t)
	var dials, lookups atomic.Int32
	pointAt(proxy, upstream.Listener.Addr().String(), &dials)
	proxy.resolver = resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	client := routedClient(t, startHTTPProxy(t, proxy), nil)
	for _, want := range []int{204, 403} {
		response, err := client.Get("http://web.example.test/")
		if err != nil {
			t.Fatal(err)
		}
		readResponse(t, response)
		if response.StatusCode != want {
			t.Fatal("解析变更未重新核验", response.StatusCode)
		}
	}
	if lookups.Load() != 2 || dials.Load() != 1 || delivered.Load() != 1 {
		t.Fatal("重复连接或复用了旧地址")
	}
	failed := testProxy(t)
	var attempts atomic.Int32
	failed.dialer = dialFunc(func(context.Context, string, string) (net.Conn, error) {
		attempts.Add(1)
		return nil, errors.New("测试连接故障")
	})
	client = routedClient(t, startHTTPProxy(t, failed), nil)
	response, err := client.Post("http://web.example.test/", "text/plain", strings.NewReader("一次写入"))
	if err != nil {
		t.Fatal(err)
	}
	readResponse(t, response)
	if response.StatusCode != 502 || attempts.Load() != 1 {
		t.Fatal("失败连接被重试")
	}
}

func TestHTTPFailuresAndLimits(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://127.0.0.1/private", 302)
			return
		}
		if r.URL.Path == "/lost" {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
			return
		}
		w.Header().Set("Content-Length", "8192")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	proxy := testProxy(t)
	pointAt(proxy, upstream.Listener.Addr().String(), &calls)
	client := routedClient(t, startHTTPProxy(t, proxy), nil)
	for _, fixture := range []struct {
		path, body string
		status     int
	}{
		{"/", strings.Repeat("x", 4097), 413}, {"/", "", 502}, {"/redirect", "", 302}, {"/lost", "", 502},
	} {
		response, err := client.Post("http://web.example.test"+fixture.path, "text/plain", strings.NewReader(fixture.body))
		if err != nil {
			t.Fatal(err)
		}
		readResponse(t, response)
		if response.StatusCode != fixture.status {
			t.Fatal("限额或错误状态不符", fixture.path, response.StatusCode)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("重定向、丢失响应或超限请求被重复执行", calls.Load())
	}
	for _, uri := range []string{"https://web.example.test/path", "http://user:ignored@web.example.test/", "http://127.0.0.1/"} {
		r := httptest.NewRequest("GET", uri, nil)
		r = r.WithContext(context.WithValue(r.Context(), authorizedKey{}, true))
		w := httptest.NewRecorder()
		proxy.handle(w, r)
		if w.Code < 400 {
			t.Fatal("接受无效协议或目标")
		}
	}
	w := httptest.NewRecorder()
	proxy.handle(w, httptest.NewRequest("GET", "http://web.example.test/", nil))
	if w.Code != 403 {
		t.Fatal("未授权处理器仍可出网")
	}
}

func TestConnectPreservesBufferedBytesAndCancel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	finished := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			finished <- err
			return
		}
		_, copyErr := io.Copy(connection, connection)
		finished <- errors.Join(copyErr, connection.Close())
	}()
	proxy := testProxy(t)
	var calls atomic.Int32
	pointAt(proxy, listener.Addr().String(), &calls)
	server := startHTTPProxy(t, proxy)
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, "CONNECT web.example.test:443 HTTP/1.1\r\nHost: web.example.test:443\r\n\r\nfirst-packet"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || response.StatusCode != 200 {
		t.Fatal("隧道建立失败", err)
	}
	content := make([]byte, len("first-packet"))
	if _, err := io.ReadFull(reader, content); err != nil || string(content) != "first-packet" {
		t.Fatal("丢失预读隧道数据", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("断开的隧道未退出")
	}
}

func TestRelayCancellationAndBound(t *testing.T) {
	for _, limit := range []bool{false, true} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			client, user := net.Pipe()
			upstream, site := net.Pipe()
			t.Cleanup(func() {
				if err := closeNetworkConnections(client, user, upstream, site); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- relay(ctx, client, client, upstream, 1024) }()
			if limit {
				var wait sync.WaitGroup
				wait.Add(2)
				go func() {
					defer wait.Done()
					if _, err := io.Copy(io.Discard, site); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						t.Error(err)
					}
				}()
				go func() {
					defer wait.Done()
					_, err := user.Write(make([]byte, 2048))
					if err == nil {
						t.Error("超限隧道完整写入")
					}
				}()
				wait.Wait()
			} else {
				cancel()
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("隧道异常被视为正常完成")
				}
			case <-time.After(time.Second):
				t.Fatal("隧道取消或限额未回收")
			}
		})
	}
}

func TestConfigurationRejectsUnboundedProxy(t *testing.T) {
	for _, change := range []func(*Options){func(o *Options) { o.Ports = nil }, func(o *Options) { o.Ports = []uint16{0} }, func(o *Options) { o.Ports = []uint16{80, 80} }, func(o *Options) { o.MaximumConnections = 0 }, func(o *Options) { o.MaximumRequestBytes = 0 }, func(o *Options) { o.IdleTimeout = 0 }, func(o *Options) { o.ShutdownTimeout = time.Millisecond }, func(o *Options) { o.DeniedNetworks = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} }} {
		options := testOptions()
		change(&options)
		if options.Validate() == nil {
			t.Fatal("接受无界或无效出网配置")
		}
	}
}
